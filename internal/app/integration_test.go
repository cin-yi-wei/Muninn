//go:build integration

// 真的 claude + 真的 muninn MCP 子程序：claude 能不能用工具替自己頻道建排程（會消耗額度）：
//
//	go test -tags integration -run Integration -v ./internal/app/
package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"muninn/internal/permission"
	"muninn/internal/runner"
	"muninn/internal/session"
)

func TestIntegrationClaudeCreatesSchedule(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "muninn")
	if out, err := exec.Command("go", "build", "-o", exe, "muninn/cmd/muninn").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	snd := &fakeSender{}
	a := withSchedules(newApp(t, filepath.Join(tmp, "state"), &fakeAsker{}, snd, nil))
	socket := filepath.Join(tmp, "p.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	go func() {
		// 只放行 Muninn 自己的工具，其他一律拒絕：絕不能讓測試裡的 claude 動到真實環境
		allowOnly := func(_ context.Context, r permission.Request) permission.Decision {
			if strings.HasPrefix(r.ToolName, "mcp__muninn__") {
				return permission.Allow(r.Input)
			}
			t.Logf("拒絕 %s %s", r.ToolName, r.Input)
			return permission.Deny("測試中只允許 Muninn 的工具")
		}
		_ = permission.ServeAll(ctx, socket, allowOnly,
			func(_ context.Context, _, m string, in json.RawMessage) permission.ToolResult {
				return a.HandleTool(m, in)
			})
	}()
	time.Sleep(100 * time.Millisecond)

	r := runner.New(runner.Config{Workdir: tmp, SessionID: session.NewUUID(), Model: "claude-haiku-5-5",
		ExtraArgs: []string{"--mcp-config", permission.MCPConfig(exe, socket, "it", false), "--permission-prompt-tool", permission.PromptTool,
			"--append-system-prompt", permission.SystemPromptFor(false), "--disallowedTools", "CronCreate,CronDelete,CronList"}})
	defer r.Close(context.Background())
	rep, err := r.Ask(ctx, "幫我在這個頻道設一個定時任務：每週一到週五早上 9 點，提醒我看昨天的 CI 結果。任務名稱用 am-ci。直接建立，不用再問我。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("claude：%s", rep.Text)
	s, err := a.Schedules.Get("am-ci")
	if err != nil {
		t.Fatalf("claude 應已透過工具建立任務：%v", err)
	}
	t.Logf("建立的任務：cron=%q tz=%q message=%q", s.Cron, s.Timezone, s.Message)
	if !strings.HasPrefix(s.Cron, "0 9 ") || !strings.Contains(s.Cron, "1-5") {
		t.Fatalf("cron 不對：%q", s.Cron)
	}
	rep, err = r.Ask(ctx, "列出這個頻道的定時任務")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("claude：%s", rep.Text)
	if !strings.Contains(rep.Text, "am-ci") {
		t.Fatal("列出時應提到 am-ci")
	}
}
