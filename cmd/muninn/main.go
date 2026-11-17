// muninn M1：單一 Discord 頻道 → 長駐 headless claude → 回話。
//
// 設定全走環境變數（M2 才會有 registry 設定檔）：
//
//	DISCORD_BOT_TOKEN       必填，bot token
//	MUNINN_CHANNEL_ID       必填，要服務的頻道 id
//	MUNINN_WORKDIR          claude 的工作目錄，預設目前目錄
//	MUNINN_STATE_DIR        佇列與 session 狀態，預設 ./state
//	MUNINN_ALLOWED_USERS    逗號分隔的 Discord user id 白名單
//	MUNINN_ALLOW_ANYONE=1   不設白名單時必須明確打開，否則拒絕啟動
//	MUNINN_MODEL            選填，claude --model
//	MUNINN_CLAUDE_BIN       選填，claude 執行檔路徑
//	MUNINN_QUOTE_REPLY=1    選填，回覆掛在原訊息底下（預設直接發一般訊息）
//	MUNINN_CLAUDE_ARGS      選填，額外 flag（以空白分隔），例如 "--permission-mode acceptEdits"
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"muninn/internal/app"
	"muninn/internal/discord"
	"muninn/internal/runner"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("muninn: %v", err)
	}
}

func run() error {
	token := os.Getenv("DISCORD_BOT_TOKEN")
	channel := os.Getenv("MUNINN_CHANNEL_ID")
	if token == "" || channel == "" {
		return fmt.Errorf("需要設定 DISCORD_BOT_TOKEN 與 MUNINN_CHANNEL_ID")
	}
	workdir := envOr("MUNINN_WORKDIR", ".")
	stateDir := envOr("MUNINN_STATE_DIR", "state")

	allowed := map[string]bool{}
	for _, u := range strings.Split(os.Getenv("MUNINN_ALLOWED_USERS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			allowed[u] = true
		}
	}
	if len(allowed) == 0 && os.Getenv("MUNINN_ALLOW_ANYONE") != "1" {
		// 頻道裡任何人都能操控 claude 是很危險的預設，要求明確表態
		return fmt.Errorf("未設定 MUNINN_ALLOWED_USERS；若真的要開放所有人，請設 MUNINN_ALLOW_ANYONE=1")
	}

	sess, resume, err := app.LoadOrCreateSession(stateDir)
	if err != nil {
		return fmt.Errorf("session 狀態: %w", err)
	}
	log.Printf("muninn: session=%s resume=%v workdir=%s", sess.ID, resume, workdir)

	r := runner.New(runner.Config{
		Bin:       os.Getenv("MUNINN_CLAUDE_BIN"),
		Workdir:   workdir,
		SessionID: sess.ID,
		Resume:    resume,
		Model:     os.Getenv("MUNINN_MODEL"),
		ExtraArgs: strings.Fields(os.Getenv("MUNINN_CLAUDE_ARGS")),
	})

	a := &app.App{
		StateDir:     stateDir,
		ChannelID:    channel,
		AllowedUsers: allowed,
		QuoteReply:   os.Getenv("MUNINN_QUOTE_REPLY") == "1",
		Asker:        r,
		Sender:       &discord.REST{Token: token},
	}
	if err := a.Init(); err != nil {
		return err
	}
	gw := &discord.Gateway{Token: token, ChannelID: channel, OnMessage: a.OnMessage}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return gw.Run(gctx) })
	g.Go(func() error { return a.RunWorker(gctx) })
	g.Go(func() error { return a.RunSender(gctx) })
	err = g.Wait()

	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.Close(closeCtx)
	if ctx.Err() != nil {
		log.Printf("muninn: 收到關機訊號，結束")
		return nil
	}
	return err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
