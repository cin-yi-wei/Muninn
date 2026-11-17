// muninn：Discord 頻道 → 每個 binding 一個長駐 headless claude → 回話。
//
// 用法：
//
//	muninn [-config muninn.json] run                       啟動所有 binding
//	muninn [-config muninn.json] check                     驗證設定並確認 worktree，不連 Discord
//	muninn [-config muninn.json] adopt [--force] <binding> 接手該 worktree 最近的既有 claude session
//	muninn [-config muninn.json] triggers list             列出定時任務與下次觸發時間
//	muninn [-config muninn.json] triggers import <檔案>    從 claude_cron 的 triggers.json 匯入（只取本設定有的 binding）
//	muninn permission-mcp --socket S --binding B           （內部用）claude 啟動的權限確認 MCP server
//
// 設定檔格式見 muninn.example.json。bot token 不放設定檔，從 token_env 指定的環境變數讀。
// 設定檔路徑預設 $MUNINN_CONFIG，再來是 ./muninn.json。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"muninn/internal/app"
	"muninn/internal/discord"
	"muninn/internal/permission"
	"muninn/internal/registry"
	"muninn/internal/runner"
	"muninn/internal/scheduler"
	"muninn/internal/session"
	"muninn/internal/worktree"
)

func main() {
	def := os.Getenv("MUNINN_CONFIG")
	if def == "" {
		def = "muninn.json"
	}
	cfgPath := flag.String("config", def, "設定檔路徑")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: muninn [-config 檔案] run | check | adopt [--force] <binding> | triggers list|import <檔案>\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	var err error
	switch flag.Arg(0) {
	case "run":
		err = cmdRun(*cfgPath)
	case "check":
		err = cmdCheck(*cfgPath)
	case "adopt":
		err = cmdAdopt(*cfgPath, flag.Args()[1:])
	case "triggers":
		err = cmdTriggers(*cfgPath, flag.Args()[1:])
	case "permission-mcp":
		err = cmdPermissionMCP(flag.Args()[1:])
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("muninn: %v", err)
	}
}

// prepare 載入設定並確保每個未暫停 binding 的 worktree 就緒。
func prepare(cfgPath string) (registry.Config, error) {
	cfg, err := registry.Load(cfgPath)
	if err != nil {
		return cfg, err
	}
	for _, b := range cfg.Bindings {
		if b.Paused {
			log.Printf("%s: 已暫停，略過", b.Name)
			continue
		}
		created, err := worktree.Ensure(b.Repo, b.Branch, b.Base, b.Worktree)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", b.Name, err)
		}
		if created {
			log.Printf("%s: 已建立 worktree %s（分支 %s）", b.Name, b.Worktree, b.Branch)
		}
	}
	return cfg, nil
}

func cmdCheck(cfgPath string) error {
	cfg, err := prepare(cfgPath)
	if err != nil {
		return err
	}
	for _, b := range cfg.Bindings {
		st := "新 session"
		if s, ok, err := session.Load(cfg.StateDirFor(b.Name)); err != nil {
			st = "session 狀態壞掉: " + err.Error()
		} else if ok {
			st = "resume " + s.ID
		}
		fmt.Printf("%-20s 頻道 %s  工作目錄 %s  分支 %q  暫停=%v  %s\n", b.Name, b.ChannelID, b.Workdir(), b.Branch, b.Paused, st)
	}
	fmt.Printf("設定 OK，共 %d 個 binding\n", len(cfg.Bindings))
	return checkTriggers(cfg)
}

// checkTriggers 驗證定時任務：cron／時區不合法、指向不存在的 binding 都列出來。
func checkTriggers(cfg registry.Config) error {
	tf, err := scheduler.Load(cfg.TriggersFile)
	if err != nil {
		return err
	}
	bad := 0
	for _, t := range tf.Triggers {
		if err := scheduler.Validate(t); err != nil {
			fmt.Printf("⚠️ trigger %v\n", err)
			bad++
		} else if _, ok := cfg.Find(t.Binding); !ok {
			fmt.Printf("⚠️ trigger %s 指向不存在的 binding %q\n", t.Name, t.Binding)
			bad++
		}
	}
	fmt.Printf("定時任務 %d 個（%s），有問題 %d 個\n", len(tf.Triggers), cfg.TriggersFile, bad)
	return nil
}

func cmdAdopt(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	force := fs.Bool("force", false, "已綁定別的 session 時強制改綁")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: muninn adopt [--force] <binding>")
	}
	cfg, err := registry.Load(cfgPath)
	if err != nil {
		return err
	}
	b, ok := cfg.Find(fs.Arg(0))
	if !ok {
		return fmt.Errorf("找不到 binding %q", fs.Arg(0))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c, err := session.FindLatest(filepath.Join(home, ".claude"), b.Workdir())
	if err != nil {
		return err
	}
	age := time.Since(c.ModTime).Round(time.Second)
	if err := session.Adopt(cfg.StateDirFor(b.Name), c, *force); err != nil {
		return err
	}
	fmt.Printf("%s 已綁定 session %s（最後寫入 %s 前）\n下一則訊息會附上交接說明。\n", b.Name, c.ID, age)
	if age < 10*time.Minute {
		// 同一個 session 同時只能一個程序在寫，兩邊同時開會把歷史寫亂
		fmt.Printf("⚠️ 這個 session %s 前還有寫入，原本的 claude_cron binding 可能還在跑。\n   啟動 Muninn 之前，務必先停掉它。\n", age)
	}
	return nil
}

func cmdRun(cfgPath string) error {
	cfg, err := prepare(cfgPath)
	if err != nil {
		return err
	}
	// token 晚點才檢查：沒有 token 也能把下面的元件接線跑一遍（缺依賴時 App.Init 會報錯）
	token := os.Getenv(cfg.TokenEnv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, gctx := errgroup.WithContext(ctx)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	// 權限確認：claude → permission-mcp 子程序 → 這個 socket → 對應 binding 的 App
	socket := filepath.Join(cfg.StateDir, "permission.sock")

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	claudeHome := filepath.Join(home, ".claude")

	sender := &discord.REST{Token: token}
	byName := map[string]*app.App{}
	apps := map[string]*app.App{} // channel id → app
	var runners []*runner.Runner
	for _, b := range cfg.Bindings {
		if b.Paused {
			continue
		}
		stateDir := cfg.StateDirFor(b.Name)
		sess, resume, err := session.LoadOrCreate(stateDir)
		if err != nil {
			return fmt.Errorf("%s: session 狀態: %w", b.Name, err)
		}
		logger := log.New(os.Stderr, "["+b.Name+"] ", log.LstdFlags)
		logger.Printf("session=%s resume=%v workdir=%s", sess.ID, resume, b.Workdir())

		extra := append([]string{
			"--mcp-config", permission.MCPConfig(exe, socket, b.Name),
			"--permission-prompt-tool", permission.PromptTool,
		}, b.ClaudeArgs...)
		sid := sess.ID
		r := runner.New(runner.Config{
			SessionExists: func() bool { return session.Exists(claudeHome, sid) },
			Bin:           cfg.ClaudeBin,
			Workdir:       b.Workdir(),
			SessionID:     sess.ID,
			Resume:        resume,
			Model:         b.Model,
			ExtraArgs:     extra,
		})
		runners = append(runners, r)
		a := &app.App{
			StateDir:          stateDir,
			ChannelID:         b.ChannelID,
			AllowedUsers:      b.Allowed(cfg),
			QuoteReply:        b.QuoteReply,
			AutoApprove:       b.AutoApprove,
			PermissionTimeout: cfg.PermTimeout(),
			Asker:             r,
			Sender:            sender,
			Prompter:          sender,
			Ctl:               r,
			SessionID:         sess.ID,
			MaxAttempts:       cfg.MaxAttempts,
			Logf:              logger.Printf,
		}
		if err := a.Init(); err != nil {
			return fmt.Errorf("%s: %w", b.Name, err)
		}
		apps[b.ChannelID] = a
		byName[b.Name] = a
		g.Go(func() error { return a.RunWorker(gctx) })
		g.Go(func() error { return a.RunSender(gctx) })
		g.Go(func() error { return a.RunSupervisor(gctx, 30*time.Second, cfg.Idle(), cfg.MemLimitBytes()) })
	}
	if len(apps) == 0 {
		return fmt.Errorf("沒有任何啟用中的 binding")
	}
	if token == "" {
		stop()
		_ = g.Wait()
		return fmt.Errorf("環境變數 %s 沒有設定 bot token（設定與接線檢查都通過了）", cfg.TokenEnv)
	}

	// 整個 bot 一條 Gateway 連線，依頻道 id 分派給對應的 binding
	gw := &discord.Gateway{
		Token:    token,
		Channels: func(id string) bool { return apps[id] != nil },
		OnMessage: func(ctx context.Context, m discord.Message) error {
			return apps[m.ChannelID].OnMessage(ctx, m)
		},
		OnInteraction: func(ctx context.Context, it discord.Interaction) {
			res := apps[it.ChannelID].HandleButton(it.CustomID, it.UserID)
			if !res.Handled {
				return
			}
			typ := discord.InteractionReply
			if res.Update {
				typ = discord.InteractionUpdateMessage
			}
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := sender.RespondInteraction(rctx, it.ID, it.Token, typ, res.Text, !res.Update); err != nil {
				log.Printf("回應按鈕互動失敗: %v", err)
			}
		},
	}
	g.Go(func() error { return gw.Run(gctx) })
	g.Go(func() error {
		// 定時任務：每 30 秒檢查一次，到期的注入對應 binding 的 inbox
		inject := func(f scheduler.Fire) (bool, error) {
			a := byName[f.Trigger.Binding]
			if a == nil {
				return false, scheduler.ErrUnknownBinding
			}
			return a.InjectScheduled(f)
		}
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			if _, err := scheduler.RunOnce(cfg.TriggersFile, time.Now(), inject, log.Printf); err != nil {
				log.Printf("scheduler: %v", err)
			}
			select {
			case <-gctx.Done():
				return gctx.Err()
			case <-t.C:
			}
		}
	})
	g.Go(func() error {
		return permission.Serve(gctx, socket, func(ctx context.Context, req permission.Request) permission.Decision {
			a := byName[req.Binding]
			if a == nil {
				return permission.Deny("未知的 binding " + req.Binding)
			}
			return a.RequestPermission(ctx, req)
		})
	})
	err = g.Wait()

	// 平行關閉所有 claude 程序
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cg errgroup.Group
	for _, r := range runners {
		cg.Go(func() error { return r.Close(closeCtx) })
	}
	_ = cg.Wait()
	if ctx.Err() != nil {
		log.Printf("muninn: 收到關機訊號，結束")
		return nil
	}
	return err
}

// cmdPermissionMCP 是 claude 透過 --mcp-config 啟動的子程序，stdio 講 MCP，決策轉給主程序。
func cmdPermissionMCP(args []string) error {
	fs := flag.NewFlagSet("permission-mcp", flag.ContinueOnError)
	socket := fs.String("socket", "", "主程序的權限 socket")
	binding := fs.String("binding", "", "binding 名稱")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" || *binding == "" {
		return fmt.Errorf("permission-mcp 需要 --socket 與 --binding")
	}
	// stdout 是 MCP 通道，log 一律走 stderr
	log.SetOutput(os.Stderr)
	return permission.ServeMCP(context.Background(), os.Stdin, os.Stdout, *socket, *binding)
}

func cmdTriggers(cfgPath string, args []string) error {
	cfg, err := registry.Load(cfgPath)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("用法: muninn triggers list | import <檔案>")
	}
	switch args[0] {
	case "list":
		tf, err := scheduler.Load(cfg.TriggersFile)
		if err != nil {
			return err
		}
		now := time.Now()
		for _, t := range tf.Triggers {
			next := "—"
			if spec, err := scheduler.ParseCron(t.Cron); err == nil {
				if loc, err := time.LoadLocation(tzOrDefault(t.Timezone)); err == nil {
					if n, ok := scheduler.NextOccurrence(spec, loc, now); ok {
						next = n.Format("01-02 15:04 MST")
					}
				}
			}
			fmt.Printf("%-28s %-20s %-18s 啟用=%-5v 補跑=%-5v 下次 %s\n", t.Name, t.Binding, t.Cron, t.Enabled, t.CatchUp, next)
		}
		fmt.Printf("共 %d 個（%s）\n", len(tf.Triggers), cfg.TriggersFile)
		return nil
	case "import":
		if len(args) != 2 {
			return fmt.Errorf("用法: muninn triggers import <claude_cron 的 triggers.json>")
		}
		src, err := scheduler.Load(args[1])
		if err != nil {
			return err
		}
		dst, err := scheduler.Load(cfg.TriggersFile)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, t := range dst.Triggers {
			have[t.Name] = true
		}
		now := time.Now().UTC().Format(time.RFC3339)
		added, skipped := 0, 0
		for _, t := range src.Triggers {
			if _, ok := cfg.Find(t.Binding); !ok || have[t.Name] {
				skipped++
				continue
			}
			if err := scheduler.Validate(t); err != nil {
				fmt.Printf("⚠️ 略過 %v\n", err)
				skipped++
				continue
			}
			// last_checked 設成現在：匯入不補跑過去錯過的時間點，從下一個排定時間開始
			t.LastChecked = now
			dst.Triggers = append(dst.Triggers, t)
			fmt.Printf("匯入 %s（binding %s，%s，啟用=%v）\n", t.Name, t.Binding, t.Cron, t.Enabled)
			added++
		}
		if added > 0 {
			if err := scheduler.Save(cfg.TriggersFile, dst); err != nil {
				return err
			}
		}
		fmt.Printf("匯入 %d 個、略過 %d 個（binding 不在本設定或同名已存在）→ %s\n", added, skipped, cfg.TriggersFile)
		if added > 0 {
			fmt.Println("⚠️ claude_cron 那邊的同名 trigger 若還開著，兩邊會各觸發一次；切換前記得停用其中一邊。")
		}
		return nil
	}
	return fmt.Errorf("不認得的子指令 %q", args[0])
}

func tzOrDefault(tz string) string {
	if tz == "" {
		return scheduler.DefaultTimezone
	}
	return tz
}
