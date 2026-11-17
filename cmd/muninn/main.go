// muninn：Discord 頻道 → 每個 binding 一個長駐 headless claude → 回話。
//
// 用法：
//
//	muninn [-config muninn.json] run                       啟動所有 binding
//	muninn [-config muninn.json] check                     驗證設定並確認 worktree，不連 Discord
//	muninn [-config muninn.json] adopt [--force] <binding> 接手該 worktree 最近的既有 claude session
//	muninn [-config muninn.json] list                      列出所有頻道
//	muninn [-config muninn.json] bind <名稱> --repo R [--branch B] [--channel ID | --create-channel | --telegram ID]
//	muninn [-config muninn.json] unbind <名稱> [--delete-channel] [--keep-worktree]
//	muninn [-config muninn.json] pause|resume <名稱>
//	muninn [-config muninn.json] set <名稱> <欄位>=<值>
//	（以上都只改設定檔，正在跑的 Muninn 會自動套用）
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
	"path/filepath"
	"strings"
	"time"

	"muninn/internal/admin"
	"muninn/internal/discord"
	"muninn/internal/permission"
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
		fmt.Fprintf(os.Stderr, "用法: muninn [-config 檔案] run | check | adopt [--force] <binding> | list | bind | unbind | pause | resume | set | schedules\n")
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
	case "list", "bind", "unbind", "pause", "resume", "set":
		err = cmdAdmin(*cfgPath, flag.Arg(0), flag.Args()[1:])
	case "schedules":
		err = cmdSchedules(*cfgPath)
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" || *binding == "" {
		return fmt.Errorf("permission-mcp 需要 --socket 與 --binding")
	}
	// stdout 是 MCP 通道，log 一律走 stderr
	log.SetOutput(os.Stderr)
	return permission.ServeMCP(context.Background(), os.Stdin, os.Stdout, *socket, *binding, *control)
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
