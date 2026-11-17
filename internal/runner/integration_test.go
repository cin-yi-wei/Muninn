//go:build integration

// 打真的 claude CLI 的整合測試（會消耗額度，預設不跑）：
//
//	go test -tags integration -run Integration -v ./internal/runner/
package runner

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"
)

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func TestIntegrationRealClaude(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sid := uuid4()
	cfg := Config{Workdir: t.TempDir(), SessionID: sid, Model: "claude-haiku-5-5"}

	r := New(cfg)
	r1, err := r.Ask(ctx, "請記住暗號是「渡鴉42」。只回覆 OK。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("第 1 輪：%q apiKeySource=%q rateLimit=%q cost≈$%.4f", r1.Text, r1.APIKeySource, r1.RateLimit, r1.CostUSD)
	pid1 := r.cmd.Process.Pid

	r2, err := r.Ask(ctx, "暗號是什麼？只回覆暗號本身。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("第 2 輪（同程序 pid=%d → %d）：%q", pid1, r.cmd.Process.Pid, r2.Text)
	if r.cmd.Process.Pid != pid1 {
		t.Fatal("第 2 輪應沿用同一個程序")
	}
	if !strings.Contains(r2.Text, "渡鴉42") {
		t.Fatalf("同程序第 2 輪應記得暗號，得到 %q", r2.Text)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// 模擬 Muninn 重啟：新的 Runner、Resume=true
	cfg.Resume = true
	r3 := New(cfg)
	defer r3.Close(context.Background())
	rep, err := r3.Ask(ctx, "再說一次暗號，只回覆暗號本身。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("重啟後 --resume：%q session=%s", rep.Text, rep.SessionID)
	if !strings.Contains(rep.Text, "渡鴉42") {
		t.Fatalf("--resume 後應記得暗號，得到 %q", rep.Text)
	}
	if rep.APIKeySource != "none" {
		t.Errorf("apiKeySource=%q：不是 none，代表可能走 API key 計費而不是訂閱", rep.APIKeySource)
	}
}
