package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"muninn/internal/admin"
	"muninn/internal/app"
	"muninn/internal/chat"
	"muninn/internal/discord"
	"muninn/internal/permission"
	"muninn/internal/registry"
	"muninn/internal/runner"
	"muninn/internal/scheduler"
	"muninn/internal/session"
	"muninn/internal/tasks"
	"muninn/internal/telegram"
	"muninn/internal/worktree"
)

// daemon 是 muninn run：管理所有 binding 的生命週期，設定檔變動時熱重載
// （新增的啟動、移除或暫停的停掉、設定改了的重啟，其他照跑不受影響）。
type daemon struct {
	cfgPath    string
	exe        string
	socket     string
	claudeHome string
	discord    *discord.REST
	tg         *telegram.Bot
	admin      *admin.Admin
	tasks      *tasks.Manager

	root context.Context

	mu      sync.RWMutex
	cfg     registry.Config
	running map[string]*liveBinding     // name → 執行中的 binding
	routes  map[string]*liveBinding     // platform:channel → 執行中的 binding
	paused  map[string]registry.Binding // platform:channel → 暫停的 binding（只回應 !resume）

	tgMu   sync.Mutex
	tgTold map[string]bool
}

type liveBinding struct {
	b      registry.Binding
	fp     string
	app    *app.App
	runner *runner.Runner
	cancel context.CancelFunc
	done   chan struct{}
}

func routeKey(platform, channel string) string { return platform + ":" + channel }

func cmdRun(cfgPath string) error {
	cfg, err := prepare(cfgPath)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	// 同一個狀態目錄只能有一個 Muninn 在跑：兩個同時跑會搶同一批 session 和佇列
	lock, err := os.OpenFile(filepath.Join(cfg.StateDir, "muninn.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("另一個 Muninn 已經在用 %s（同時只能跑一個）", cfg.StateDir)
	}
	token := os.Getenv(cfg.TokenEnv)
	tgToken := os.Getenv(cfg.TelegramTokenEnv)
	d := &daemon{
		cfgPath:    cfgPath,
		exe:        exe,
		socket:     filepath.Join(cfg.StateDir, "permission.sock"),
		claudeHome: filepath.Join(home, ".claude"),
		discord:    &discord.REST{Token: token},
		tg:         &telegram.Bot{Token: tgToken},
		cfg:        cfg,
		running:    map[string]*liveBinding{},
		routes:     map[string]*liveBinding{},
		paused:     map[string]registry.Binding{},
		tgTold:     map[string]bool{},
	}
	d.admin = &admin.Admin{ConfigPath: cfgPath}
	if token != "" {
		d.admin.Discord = d.discord
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d.root = ctx
	g, gctx := errgroup.WithContext(ctx)

	// 委派任務：每個任務一個獨立的 headless claude，權限確認轉到委派者的頻道
	d.tasks = &tasks.Manager{
		StateDir: filepath.Join(cfg.StateDir, "tasks"), MaxRunning: cfg.MaxTasks, TaskTimeout: cfg.TaskLimit(),
		NewAsker: d.newTaskAsker,
		Deliver: func(source, id, text string) error {
			a := d.byName(source)
			if a == nil {
				return fmt.Errorf("binding %s 已停止", source)
			}
			return a.DeliverTaskResult(id, text)
		},
		Notify: func(source, text string) {
			if a := d.byName(source); a != nil {
				a.Notify(text)
			}
		},
		Logf: log.Printf,
	}
	if err := d.tasks.Start(ctx); err != nil {
		return err
	}

	// 先把所有 binding 接線一遍（缺依賴時 App.Init 會報錯），再檢查 token
	d.apply(cfg)
	var missing []string
	for _, b := range cfg.Bindings {
		if b.Platform == registry.PlatformTelegram && tgToken == "" {
			missing = append(missing, cfg.TelegramTokenEnv)
		} else if b.Platform == registry.PlatformDiscord && token == "" {
			missing = append(missing, cfg.TokenEnv)
		}
	}
	if len(missing) > 0 {
		d.stopAll()
		return fmt.Errorf("環境變數 %s 沒有設定 bot token（設定與接線檢查都通過了）", strings.Join(uniq(missing), "、"))
	}
	if token == "" && tgToken == "" {
		d.stopAll()
		return fmt.Errorf("沒有任何 bot token（%s / %s）", cfg.TokenEnv, cfg.TelegramTokenEnv)
	}

	// 有 token 就連線：之後新增的 binding 不用重連
	if token != "" {
		gw := &discord.Gateway{
			Token:    token,
			Channels: func(id string) bool { return d.accepts(registry.PlatformDiscord, id) },
			OnMessage: func(ctx context.Context, m discord.Message) error {
				return d.onMessage(ctx, registry.PlatformDiscord, m)
			},
			OnInteraction: d.onDiscordInteraction,
		}
		g.Go(func() error { return gw.Run(gctx) })
	}
	if tgToken != "" {
		poller := &telegram.Poller{
			Bot:           d.tg,
			OffsetFile:    filepath.Join(cfg.StateDir, "telegram_offset"),
			Chats:         func(id string) bool { return d.accepts(registry.PlatformTelegram, id) },
			OnMessage:     func(ctx context.Context, m chat.Message) error { return d.onMessage(ctx, registry.PlatformTelegram, m) },
			OnUnknown:     d.onTelegramUnknown,
			OnInteraction: d.onTelegramInteraction,
		}
		g.Go(func() error { return poller.Run(gctx) })
	}
	g.Go(func() error {
		return permission.ServeAll(gctx, d.socket, d.handlePermission, d.handleTool)
	})
	g.Go(func() error { return d.watchConfig(gctx) })

	err = g.Wait()
	d.stopAll()
	if ctx.Err() != nil {
		log.Printf("muninn: 收到關機訊號，結束")
		return nil
	}
	return err
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// fingerprint 是會影響一個 binding 執行方式的所有設定；變了就要重啟它。
func fingerprint(cfg registry.Config, b registry.Binding) string {
	v := struct {
		B                     registry.Binding
		Allowed               map[string]bool
		Perm, Idle, Stall, TZ string
		Mem                   int64
		Max                   int
		Bin                   string
	}{b, b.Allowed(cfg), cfg.PermissionTimeout, cfg.IdleTimeout, cfg.StallTimeout, cfg.Timezone, cfg.MemLimitBytes(), cfg.MaxAttempts, cfg.ClaudeBin}
	data, _ := json.Marshal(v)
	return string(data)
}

// apply 讓執行中的 binding 跟 cfg 一致。
func (d *daemon) apply(cfg registry.Config) {
	want := map[string]registry.Binding{}
	paused := map[string]registry.Binding{}
	for _, b := range cfg.Bindings {
		if b.Paused {
			paused[routeKey(b.Platform, b.ChannelID)] = b
			continue
		}
		want[b.Name] = b
	}
	d.mu.Lock()
	d.cfg = cfg
	d.paused = paused
	var stops []*liveBinding
	for name, lb := range d.running {
		nb, ok := want[name]
		if !ok || fingerprint(cfg, nb) != lb.fp {
			stops = append(stops, lb)
			delete(d.running, name)
			delete(d.routes, routeKey(lb.b.Platform, lb.b.ChannelID))
		}
	}
	d.mu.Unlock()

	for _, lb := range stops {
		d.stopBinding(lb)
	}
	for name, b := range want {
		d.mu.RLock()
		_, ok := d.running[name]
		d.mu.RUnlock()
		if ok {
			continue
		}
		if _, err := worktree.Ensure(b.Repo, b.Branch, b.Base, b.Worktree); err != nil {
			log.Printf("[%s] worktree 有問題，不啟動: %v", name, err)
			continue
		}
		lb, err := d.startBinding(cfg, b)
		if err != nil {
			log.Printf("[%s] 啟動失敗: %v", name, err)
			continue
		}
		d.mu.Lock()
		d.running[name] = lb
		d.routes[routeKey(b.Platform, b.ChannelID)] = lb
		d.mu.Unlock()
	}
}

func (d *daemon) startBinding(cfg registry.Config, b registry.Binding) (*liveBinding, error) {
	stateDir := cfg.StateDirFor(b.Name)
	sess, resume, err := session.LoadOrCreate(stateDir)
	if err != nil {
		return nil, fmt.Errorf("session 狀態: %w", err)
	}
	if err := os.MkdirAll(app.AttachmentsDir(stateDir), 0o755); err != nil {
		return nil, err
	}
	logger := log.New(os.Stderr, "["+b.Name+"] ", log.LstdFlags)
	logger.Printf("啟動 session=%s resume=%v workdir=%s", sess.ID, resume, b.Workdir())

	extra := append([]string{
		"--mcp-config", permission.MCPConfig(d.exe, d.socket, b.Name, b.Control),
		"--permission-prompt-tool", permission.PromptTool,
		"--append-system-prompt", permission.SystemPromptFor(b.Control),
		// claude 內建的 Cron 工具只活在這個程序裡，閒置回收或重啟就消失，跟 Muninn 的排程搞混只會出事
		"--disallowedTools", "CronCreate,CronDelete,CronList",
		// 使用者傳來的附件下載在狀態目錄底下，要讓 claude 讀得到
		"--add-dir", app.AttachmentsDir(stateDir),
	}, b.ClaudeArgs...)
	sid := sess.ID
	r := runner.New(runner.Config{
		SessionExists: func() bool { return session.Exists(d.claudeHome, sid) },
		Bin:           cfg.ClaudeBin,
		Workdir:       b.Workdir(),
		SessionID:     sess.ID,
		Resume:        resume,
		Model:         b.Model,
		ExtraArgs:     extra,
	})
	var out interface {
		app.Sender
		app.Prompter
		app.Files
	} = d.discord
	if b.Platform == registry.PlatformTelegram {
		out = d.tg
	}
	a := &app.App{
		StateDir:          stateDir,
		ChannelID:         b.ChannelID,
		AllowedUsers:      b.Allowed(cfg),
		QuoteReply:        b.QuoteReply,
		AutoApprove:       b.AutoApprove,
		PermissionTimeout: cfg.PermTimeout(),
		Asker:             r,
		Sender:            out,
		Prompter:          out,
		Files:             out,
		Ctl:               r,
		SessionID:         sess.ID,
		Schedules:         scheduler.NewStore(stateDir, cfg.Timezone),
		Activity:          b.ActivityOn(),
		Workdir:           b.Workdir(),
		Repo:              b.Repo,
		MaxAttempts:       cfg.MaxAttempts,
		StallTimeout:      cfg.Stall(),
		Logf:              logger.Printf,
	}
	a.Name, a.Pauser = b.Name, d.admin
	if b.Control {
		a.Admin = d.admin
	}
	if d.tasks != nil {
		a.Tasks = d.tasks
	}
	if err := a.Init(); err != nil {
		return nil, err
	}
	if err := session.MarkRunning(stateDir); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(d.root)
	done := make(chan struct{})
	var g errgroup.Group
	g.Go(func() error { return a.RunWorker(ctx) })
	g.Go(func() error { return a.RunSender(ctx) })
	g.Go(func() error { return a.RunScheduler(ctx, 30*time.Second) })
	g.Go(func() error { return a.RunSupervisor(ctx, 30*time.Second, cfg.Idle(), cfg.MemLimitBytes()) })
	go func() {
		_ = g.Wait()
		close(done)
	}()
	return &liveBinding{b: b, fp: fingerprint(cfg, b), app: a, runner: r, cancel: cancel, done: done}, nil
}

func (d *daemon) stopBinding(lb *liveBinding) {
	log.Printf("[%s] 停止", lb.b.Name)
	lb.cancel()
	<-lb.done
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = lb.runner.Close(ctx)
	session.ClearRunning(d.cfg.StateDirFor(lb.b.Name))
}

func (d *daemon) stopAll() {
	d.mu.Lock()
	all := make([]*liveBinding, 0, len(d.running))
	for _, lb := range d.running {
		all = append(all, lb)
	}
	d.running = map[string]*liveBinding{}
	d.routes = map[string]*liveBinding{}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, lb := range all {
		wg.Add(1)
		go func() { defer wg.Done(); d.stopBinding(lb) }()
	}
	wg.Wait()
}

// watchConfig 每 3 秒看一次設定檔，有變動就重新載入；SIGHUP 也會觸發。
func (d *daemon) watchConfig(ctx context.Context) error {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	stamp := func() string {
		info, err := os.Stat(d.cfgPath)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size())
	}
	last := stamp()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hup:
		case <-t.C:
			if s := stamp(); s == last {
				continue
			}
		}
		last = stamp()
		cfg, err := registry.Load(d.cfgPath)
		if err != nil {
			log.Printf("muninn: 設定檔有錯，維持原本的設定: %v", err)
			continue
		}
		d.mu.RLock()
		old := d.cfg
		d.mu.RUnlock()
		if old.StateDir != cfg.StateDir || old.TokenEnv != cfg.TokenEnv || old.TelegramTokenEnv != cfg.TelegramTokenEnv {
			log.Printf("muninn: state_dir / token_env 的變動要重啟 Muninn 才會生效")
		}
		log.Printf("muninn: 設定檔變動，重新載入")
		d.apply(cfg)
	}
}

// ---------- 訊息分派 ----------

func (d *daemon) live(platform, channel string) *liveBinding {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.routes[routeKey(platform, channel)]
}

func (d *daemon) accepts(platform, channel string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	k := routeKey(platform, channel)
	_, paused := d.paused[k]
	return d.routes[k] != nil || paused
}

func (d *daemon) onMessage(ctx context.Context, platform string, m chat.Message) error {
	if lb := d.live(platform, m.ChannelID); lb != nil {
		return lb.app.OnMessage(ctx, m)
	}
	// 暫停中的頻道：只認 !resume（白名單內的人）
	d.mu.RLock()
	b, ok := d.paused[routeKey(platform, m.ChannelID)]
	cfg := d.cfg
	d.mu.RUnlock()
	if !ok {
		return nil
	}
	allowed := b.Allowed(cfg)
	if len(allowed) > 0 && !allowed[m.AuthorID] {
		return nil
	}
	cmd := strings.ToLower(strings.TrimSpace(m.Content))
	if cmd != "!resume" && !strings.HasPrefix(cmd, "/resume") && cmd != "!status" && !strings.HasPrefix(cmd, "/status") {
		return nil
	}
	reply := "⏸ 這個頻道暫停中，打 `!resume` 恢復"
	if strings.Contains(cmd, "resume") {
		msg, err := d.admin.SetPaused(b.Name, false)
		reply = "▶️ " + msg
		if err != nil {
			reply = "恢復失敗：" + err.Error()
		}
	}
	var out app.Sender = d.discord
	if platform == registry.PlatformTelegram {
		out = d.tg
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return out.Send(sctx, m.ChannelID, reply, "")
}

func (d *daemon) onDiscordInteraction(ctx context.Context, it discord.Interaction) {
	lb := d.live(registry.PlatformDiscord, it.ChannelID)
	if lb == nil {
		return
	}
	res := lb.app.HandleButton(it.CustomID, it.UserID)
	if !res.Handled {
		return
	}
	typ := discord.InteractionReply
	if res.Update {
		typ = discord.InteractionUpdateMessage
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.discord.RespondInteraction(rctx, it.ID, it.Token, typ, res.Text, !res.Update); err != nil {
		log.Printf("回應按鈕互動失敗: %v", err)
	}
}

func (d *daemon) onTelegramInteraction(ctx context.Context, it chat.Interaction) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lb := d.live(registry.PlatformTelegram, it.ChannelID)
	var res app.ButtonResult
	if lb != nil {
		res = lb.app.HandleButton(it.CustomID, it.UserID)
	}
	// Telegram 一定要回應按鈕，否則會一直轉圈
	if res.Handled && res.Update {
		if err := d.tg.EditMessage(rctx, it.ChannelID, it.MessageID, res.Text); err != nil {
			log.Printf("telegram: 改寫確認訊息失敗: %v", err)
		}
		res.Text = ""
	}
	if err := d.tg.AnswerCallback(rctx, it.ID, res.Text, res.Handled && !res.Update); err != nil {
		log.Printf("telegram: 回應按鈕失敗: %v", err)
	}
}

func (d *daemon) onTelegramUnknown(ctx context.Context, m chat.Message) {
	log.Printf("telegram: 未綁定的聊天室 chat_id=%s user_id=%s 傳了訊息", m.ChannelID, m.AuthorID)
	d.tgMu.Lock()
	first := !d.tgTold[m.ChannelID]
	d.tgTold[m.ChannelID] = true
	d.tgMu.Unlock()
	if !first {
		return
	}
	msg := fmt.Sprintf("👋 Muninn 收到了，但這個聊天室還沒綁定。\nchat id：`%s`\n你的 user id：`%s`\n綁定方式：在控制頻道打 `!bind <名稱> <repo> <分支> --telegram=%s`，或在機器上執行 `muninn bind`。", m.ChannelID, m.AuthorID, m.ChannelID)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.tg.Send(sctx, m.ChannelID, msg, m.ID); err != nil {
		log.Printf("telegram: 回覆未綁定聊天室失敗: %v", err)
	}
}

// ---------- 權限與工具 ----------

func (d *daemon) byName(name string) *app.App {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if lb := d.running[name]; lb != nil {
		return lb.app
	}
	return nil
}

func (d *daemon) handlePermission(ctx context.Context, req permission.Request) permission.Decision {
	if id, ok := strings.CutPrefix(req.Binding, "task:"); ok {
		return d.taskPermission(ctx, id, req)
	}
	a := d.byName(req.Binding)
	if a == nil {
		return permission.Deny("binding " + req.Binding + " 已停止")
	}
	return a.RequestPermission(ctx, req)
}

func (d *daemon) handleTool(ctx context.Context, binding, method string, input json.RawMessage) permission.ToolResult {
	if strings.HasPrefix(binding, "task:") {
		return permission.ToolResult{Text: "委派任務不能用頻道工具", IsError: true}
	}
	a := d.byName(binding)
	if a == nil {
		return permission.ToolResult{Text: "binding " + binding + " 已停止", IsError: true}
	}
	return a.HandleTool(method, input)
}

// newTaskAsker 建立跑委派任務的 claude：工作目錄是任務 worktree，MCP 只有權限確認。
func (d *daemon) newTaskAsker(t tasks.Task, extra []string) tasks.Asker {
	d.mu.RLock()
	cfg := d.cfg
	src, ok := cfg.Find(t.Source)
	d.mu.RUnlock()
	model := ""
	if ok {
		model = src.Model
	}
	args := append([]string{
		"--mcp-config", permission.TaskMCPConfig(d.exe, d.socket, "task:"+t.ID),
		"--strict-mcp-config", // 任務只掛 Muninn 的權限 MCP，不帶使用者其他的 MCP server
		"--permission-prompt-tool", permission.PromptTool,
	}, extra...)
	sid := t.SessionID
	return runner.New(runner.Config{
		SessionExists: func() bool { return session.Exists(d.claudeHome, sid) },
		Bin:           cfg.ClaudeBin,
		Workdir:       t.Worktree,
		SessionID:     t.SessionID,
		Model:         model,
		ExtraArgs:     args,
	})
}

// taskPermission 判斷委派任務的工具呼叫：範圍內的直接放行；要問的轉到委派者的頻道
// （readonly 任務不轉，直接拒絕）。
func (d *daemon) taskPermission(ctx context.Context, id string, req permission.Request) permission.Decision {
	t, ok := d.tasks.Lookup(id)
	if !ok || !t.Active() {
		return permission.Deny("任務 " + id + " 已結束")
	}
	if t.Level == tasks.LevelReadonly && req.ToolName == "Bash" {
		// readonly 任務的 Bash 只放行唯讀指令（寫檔工具已停用，但 Bash 也能寫檔）
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(req.Input, &in)
		if permission.ReadonlyBash(in.Command) {
			return permission.Allow(req.Input)
		}
		log.Printf("[task %s] readonly 拒絕 Bash：%s", id, in.Command)
		return permission.Deny("readonly 任務只能跑唯讀指令（ls、cat、rg、git log/diff/show…），不能寫檔或執行其他程式")
	}
	scope := permission.Scope{Workdir: t.Worktree, Repo: t.Repo}
	ask, why := permission.NeedsApproval(req.ToolName, req.Input, scope)
	if !ask {
		log.Printf("[task %s] 放行 %s（%s）", id, req.ToolName, why)
		return permission.Allow(req.Input)
	}
	if t.Level == tasks.LevelReadonly {
		return permission.Deny("readonly 任務不能做這個操作（" + req.ToolName + "）；需要的話請委派者改用 develop 重新委派")
	}
	a := d.byName(t.Source)
	if a == nil {
		return permission.Deny("委派者的頻道已停止，沒有人能核准")
	}
	req.Label = fmt.Sprintf("委派任務 `%s`（%s）", id, t.Title)
	return a.RequestPermissionAsked(ctx, req)
}
