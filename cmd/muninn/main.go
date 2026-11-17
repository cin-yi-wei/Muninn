// muninn：Discord 頻道 → 每個 binding 一個長駐 headless claude → 回話。
//
// 用法：
//
//	muninn [-config muninn.json] run                       啟動所有 binding
//	muninn [-config muninn.json] check                     驗證設定、確認 worktree、檢查環境（claude、git、token、登入），不連線
//	muninn version
//	muninn [-config muninn.json] adopt [--force] <binding> 接手該 worktree 最近的既有 claude session
//	muninn [-config muninn.json] list                      列出所有頻道
//	muninn [-config muninn.json] bind <名稱> --repo R [--branch B] [--channel ID | --create-channel | --telegram ID]
//	muninn [-config muninn.json] unbind <名稱> [--delete-channel] [--keep-worktree]
//	muninn [-config muninn.json] pause|resume <名稱>
//	muninn [-config muninn.json] set <名稱> <欄位>=<值>
//	（以上都只改設定檔，正在跑的 Muninn 會自動套用）
//	muninn [-config muninn.json] schedule add <頻道> <名稱> (--cron "0 9 * * 1-5" | --at "2026-11-18 09:00") --message "…"
//	muninn [-config muninn.json] schedule rm|pause|resume|run <頻道> <名稱>
//	muninn [-config muninn.json] notify <頻道> <文字>       從外部腳本在頻道貼一則訊息（給長時間背景工作通知用）
//	muninn [-config muninn.json] schedules                 列出各頻道的定時任務（新增、修改由頻道裡的 claude 透過工具處理）
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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"muninn/internal/admin"
	"muninn/internal/app"
	"muninn/internal/discord"
	"muninn/internal/permission"
	"muninn/internal/queue"
	"muninn/internal/registry"
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
		fmt.Fprintf(os.Stderr, "用法: muninn [-config 檔案] run | check | adopt [--force] <binding> | version | list | bind | unbind | pause | resume | set | schedules | schedule | notify\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	var err error
	switch flag.Arg(0) {
	case "run":
		err = cmdRun(*cfgPath)
	case "version":
		fmt.Println("muninn " + version)
		return
	case "check":
		err = cmdCheck(*cfgPath)
	case "adopt":
		err = cmdAdopt(*cfgPath, flag.Args()[1:])
	case "list", "bind", "unbind", "pause", "resume", "set":
		err = cmdAdmin(*cfgPath, flag.Arg(0), flag.Args()[1:])
	case "schedules":
		err = cmdSchedules(*cfgPath)
	case "schedule":
		err = cmdSchedule(*cfgPath, flag.Args()[1:])
	case "notify":
		err = cmdNotify(*cfgPath, flag.Args()[1:])
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
	return checkEnv(cfg)
}

// version 由建置時注入：go build -ldflags "-X main.version=$(git describe --always --dirty)"
var version = "dev"

// checkEnv 檢查執行環境（兼 doctor）：工具在不在 PATH、token 有沒有設、claude 有沒有登入、狀態目錄能不能寫。
// 有問題時列出來並回傳錯誤（不會實際呼叫 claude，不耗額度）。
func checkEnv(cfg registry.Config) error {
	var problems []string
	ok := func(format string, args ...any) { fmt.Printf("✅ "+format+"\n", args...) }
	bad := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		problems = append(problems, msg)
		fmt.Println("❌ " + msg)
	}
	bin := cfg.ClaudeBin
	if bin == "" {
		bin = "claude"
	}
	if p, err := exec.LookPath(bin); err != nil {
		bad("找不到 claude 執行檔（%s）", bin)
	} else if out, err := exec.Command(p, "--version").Output(); err != nil {
		bad("claude --version 失敗：%v", err)
	} else {
		ok("claude %s（%s）", strings.TrimSpace(string(out)), p)
	}
	if p, err := exec.LookPath("git"); err != nil {
		bad("找不到 git")
	} else {
		ok("git（%s）", p)
	}
	need := map[string]string{}
	for _, b := range cfg.Bindings {
		if b.Platform == registry.PlatformTelegram {
			need[cfg.TelegramTokenEnv] = "Telegram"
		} else {
			need[cfg.TokenEnv] = "Discord"
		}
	}
	for env, plat := range need {
		if os.Getenv(env) == "" {
			bad("%s bot token 沒設定（環境變數 %s）", plat, env)
		} else {
			ok("%s bot token 已設定（%s）", plat, env)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		cred := filepath.Join(home, ".claude", ".credentials.json")
		if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
			cred = filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json")
		}
		if _, err := os.Stat(cred); err != nil && os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
			bad("看不到 claude 的登入資料（%s），請在這台機器執行 claude 並 /login", cred)
		} else {
			ok("claude 登入資料存在")
		}
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		bad("狀態目錄無法建立：%v", err)
	} else if f, err := os.CreateTemp(cfg.StateDir, ".check-*"); err != nil {
		bad("狀態目錄不能寫：%v", err)
	} else {
		f.Close()
		os.Remove(f.Name())
		ok("狀態目錄可寫（%s）", cfg.StateDir)
	}
	if len(problems) > 0 {
		return fmt.Errorf("環境有 %d 個問題", len(problems))
	}
	fmt.Println("環境 OK")
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
		fmt.Printf("⚠️ 這個 session %s 前還有寫入，可能還有別的程序在用它。\n   同一個 session 同時只能有一個程序在寫，啟動 Muninn 之前先確認。\n", age)
	}
	return nil
}

// cmdPermissionMCP 是 claude 透過 --mcp-config 啟動的子程序，stdio 講 MCP，決策轉給主程序。
func cmdPermissionMCP(args []string) error {
	fs := flag.NewFlagSet("permission-mcp", flag.ContinueOnError)
	socket := fs.String("socket", "", "主程序的權限 socket")
	binding := fs.String("binding", "", "binding 名稱")
	control := fs.Bool("control", false, "控制頻道：多掛管理 binding 的工具")
	noTools := fs.Bool("no-tools", false, "委派任務用：只有權限確認，沒有頻道工具")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" || *binding == "" {
		return fmt.Errorf("permission-mcp 需要 --socket 與 --binding")
	}
	// stdout 是 MCP 通道，log 一律走 stderr
	log.SetOutput(os.Stderr)
	return permission.ServeMCP(context.Background(), os.Stdin, os.Stdout, *socket, *binding, *control, *noTools)
}

func cmdSchedules(cfgPath string) error {
	cfg, err := registry.Load(cfgPath)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, b := range cfg.Bindings {
		st := scheduler.NewStore(cfg.StateDirFor(b.Name), cfg.Timezone)
		list, err := st.List()
		if err != nil {
			return fmt.Errorf("%s: %w", b.Name, err)
		}
		fmt.Printf("%s（%d 個）\n", b.Name, len(list))
		for _, s := range list {
			fmt.Println("  " + scheduler.Describe(s, cfg.Timezone, now))
		}
	}
	return nil
}

func cmdAdmin(cfgPath, sub string, args []string) error {
	cfg, err := registry.Load(cfgPath)
	if err != nil {
		return err
	}
	adm := &admin.Admin{ConfigPath: cfgPath}
	if tok := os.Getenv(cfg.TokenEnv); tok != "" {
		adm.Discord = &discord.REST{Token: tok}
	}
	var msg string
	switch sub {
	case "list":
		msg, err = adm.List()
	case "pause", "resume":
		if len(args) != 1 {
			return fmt.Errorf("用法: muninn %s <名稱>", sub)
		}
		msg, err = adm.SetPaused(args[0], sub == "pause")
	case "set":
		if len(args) != 2 || !strings.Contains(args[1], "=") {
			return fmt.Errorf("用法: muninn set <名稱> <欄位>=<值>")
		}
		k, v, _ := strings.Cut(args[1], "=")
		msg, err = adm.Set(args[0], k, v)
	case "bind":
		fs := flag.NewFlagSet("bind", flag.ContinueOnError)
		o := admin.BindOptions{}
		fs.StringVar(&o.Repo, "repo", "", "主 repo 路徑（必填）")
		fs.StringVar(&o.Branch, "branch", "", "分支；給了會建 worktree")
		fs.StringVar(&o.Base, "base", "", "分支不存在時從哪裡開")
		fs.StringVar(&o.Worktree, "worktree", "", "worktree 路徑，預設 <worktree_root>/<名稱>")
		fs.StringVar(&o.ChannelID, "channel", "", "已存在的 Discord 頻道 id")
		tgID := fs.String("telegram", "", "Telegram chat id")
		fs.BoolVar(&o.CreateChannel, "create-channel", false, "自動建同名 Discord 頻道")
		fs.BoolVar(&o.AutoApprove, "auto-approve", false, "權限請求直接放行")
		fs.BoolVar(&o.Control, "control", false, "設為控制頻道")
		fs.StringVar(&o.Model, "model", "", "claude 模型")
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return fmt.Errorf("用法: muninn bind <名稱> --repo R [--branch B] [--channel ID | --create-channel | --telegram ID]")
		}
		o.Name = args[0]
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *tgID != "" {
			o.Platform, o.ChannelID = registry.PlatformTelegram, *tgID
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		msg, err = adm.Bind(ctx, o)
	case "unbind":
		fs := flag.NewFlagSet("unbind", flag.ContinueOnError)
		o := admin.UnbindOptions{}
		fs.BoolVar(&o.DeleteChannel, "delete-channel", false, "連 Discord 頻道一起刪")
		fs.BoolVar(&o.KeepWorktree, "keep-worktree", false, "保留 worktree")
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return fmt.Errorf("用法: muninn unbind <名稱> [--delete-channel] [--keep-worktree]")
		}
		o.Name = args[0]
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		msg, err = adm.Unbind(ctx, o)
	}
	if err != nil {
		return err
	}
	fmt.Println(msg)
	return nil
}

// cmdSchedule 讓人不透過 claude 直接管理定時任務（改 schedules.json，正在跑的 Muninn 30 秒內會看到）。
func cmdSchedule(cfgPath string, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("用法: muninn schedule add|rm|pause|resume|run <頻道> <名稱> …")
	}
	cfg, err := registry.Load(cfgPath)
	if err != nil {
		return err
	}
	sub, name, task := args[0], args[1], args[2]
	b, ok := cfg.Find(name)
	if !ok {
		return fmt.Errorf("找不到頻道 %q", name)
	}
	st := scheduler.NewStore(cfg.StateDirFor(name), cfg.Timezone)
	now := time.Now()
	switch sub {
	case "add":
		fs := flag.NewFlagSet("schedule add", flag.ContinueOnError)
		s := scheduler.Schedule{Name: task}
		fs.StringVar(&s.Cron, "cron", "", "重複任務的 cron")
		fs.StringVar(&s.At, "at", "", "單次任務的時間")
		fs.StringVar(&s.Message, "message", "", "時間到時送進頻道的內容")
		fs.StringVar(&s.Timezone, "timezone", "", "時區")
		fs.BoolVar(&s.CatchUp, "catch-up", false, "錯過時補跑")
		replace := fs.Bool("replace", false, "同名時覆蓋")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		added, err := st.Add(s, *replace, now)
		if err != nil {
			return err
		}
		fmt.Println("已建立：" + scheduler.Describe(added, cfg.Timezone, now))
	case "rm":
		if err := st.Remove(task); err != nil {
			return err
		}
		fmt.Println("已刪除 " + task)
	case "pause", "resume":
		if err := st.SetPaused(task, sub == "pause", now); err != nil {
			return err
		}
		fmt.Printf("已%s %s\n", map[string]string{"pause": "暫停", "resume": "恢復"}[sub], task)
	case "run":
		s, err := st.Get(task)
		if err != nil {
			return err
		}
		// 直接寫進那個頻道的 inbox，正在跑的 Muninn 幾秒內會處理
		q := queue.Dir{Root: filepath.Join(cfg.StateDirFor(name), "inbox")}
		if err := q.Init(); err != nil {
			return err
		}
		id := now.UTC().Format("20060102T150405.000000000") + "-cli-" + task
		job := app.InboxJob{ID: id, ChannelID: b.ChannelID, MessageID: "cli-" + task, AuthorID: app.SchedulerAuthor,
			Content:   fmt.Sprintf("[Muninn 定時任務「%s」（手動觸發）]\n%s", task, s.Message),
			CreatedAt: now.UTC().Format(time.RFC3339)}
		if err := q.Put(id, job); err != nil {
			return err
		}
		fmt.Println("已觸發 " + task + "（Muninn 有在跑的話幾秒內會處理）")
	default:
		return fmt.Errorf("不認得的子指令 %q", sub)
	}
	return nil
}

// cmdNotify 在頻道貼一則訊息：寫進那個頻道的 outbox，正在跑的 Muninn 幾秒內送出（不經過 claude）。
func cmdNotify(cfgPath string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("用法: muninn notify <頻道> <文字>")
	}
	cfg, err := registry.Load(cfgPath)
	if err != nil {
		return err
	}
	b, ok := cfg.Find(args[0])
	if !ok {
		return fmt.Errorf("找不到頻道 %q", args[0])
	}
	q := queue.Dir{Root: filepath.Join(cfg.StateDirFor(b.Name), "outbox")}
	if err := q.Init(); err != nil {
		return err
	}
	now := time.Now().UTC()
	id := now.Format("20060102T150405.000000000") + "-notify"
	if err := q.Put(id, app.OutboxJob{ID: id, ChannelID: b.ChannelID, Text: strings.Join(args[1:], " ")}); err != nil {
		return err
	}
	fmt.Println("已排入，Muninn 幾秒內送出")
	return nil
}
