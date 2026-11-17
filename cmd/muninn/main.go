// muninn：Discord 頻道 → 每個 binding 一個長駐 headless claude → 回話。
//
// 用法：
//
//	muninn [-config muninn.json] run                       啟動所有 binding
//	muninn [-config muninn.json] check                     驗證設定並確認 worktree，不連 Discord
//	muninn [-config muninn.json] adopt [--force] <binding> 接手該 worktree 最近的既有 claude session
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
	"muninn/internal/registry"
	"muninn/internal/runner"
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
		fmt.Fprintf(os.Stderr, "用法: muninn [-config 檔案] run | check | adopt [--force] <binding>\n")
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
	token := os.Getenv(cfg.TokenEnv)
	if token == "" {
		return fmt.Errorf("環境變數 %s 沒有設定 bot token", cfg.TokenEnv)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, gctx := errgroup.WithContext(ctx)

	sender := &discord.REST{Token: token}
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

		r := runner.New(runner.Config{
			Bin:       cfg.ClaudeBin,
			Workdir:   b.Workdir(),
			SessionID: sess.ID,
			Resume:    resume,
			Model:     b.Model,
			ExtraArgs: b.ClaudeArgs,
		})
		runners = append(runners, r)
		a := &app.App{
			StateDir:     stateDir,
			ChannelID:    b.ChannelID,
			AllowedUsers: b.Allowed(cfg),
			QuoteReply:   b.QuoteReply,
			Asker:        r,
			Sender:       sender,
			Logf:         logger.Printf,
		}
		if err := a.Init(); err != nil {
			return fmt.Errorf("%s: %w", b.Name, err)
		}
		apps[b.ChannelID] = a
		g.Go(func() error { return a.RunWorker(gctx) })
		g.Go(func() error { return a.RunSender(gctx) })
	}
	if len(apps) == 0 {
		return fmt.Errorf("沒有任何啟用中的 binding")
	}

	// 整個 bot 一條 Gateway 連線，依頻道 id 分派給對應的 binding
	gw := &discord.Gateway{
		Token:    token,
		Channels: func(id string) bool { return apps[id] != nil },
		OnMessage: func(ctx context.Context, m discord.Message) error {
			return apps[m.ChannelID].OnMessage(ctx, m)
		},
	}
	g.Go(func() error { return gw.Run(gctx) })
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
