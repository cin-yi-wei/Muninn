//go:build integration

// 用真的 claude + 真的 muninn permission-mcp 子程序驗證權限確認（會消耗額度）：
//
//	go test -tags integration -run Integration -v ./internal/permission/
package permission

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntegrationPermissionPromptTool(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "muninn")
	if out, err := exec.Command("go", "build", "-o", exe, "muninn/cmd/muninn").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	socket := filepath.Join(tmp, "p.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	var mu sync.Mutex
	var seen []Request
	go func() {
		_ = Serve(ctx, socket, func(_ context.Context, r Request) Decision {
			mu.Lock()
			seen = append(seen, r)
			mu.Unlock()
			if strings.Contains(string(r.Input), "denied.txt") {
				return Deny("使用者拒絕了這個操作")
			}
			return Allow(nil)
		})
	}()
	time.Sleep(100 * time.Millisecond)

	run := func(prompt string) string {
		work := filepath.Join(tmp, "work")
		_ = os.MkdirAll(work, 0o755)
		cmd := exec.CommandContext(ctx, "claude", "-p", "--model", "claude-haiku-5-5",
			"--mcp-config", MCPConfig(exe, socket, "it"),
			"--permission-prompt-tool", PromptTool, prompt)
		cmd.Dir = work
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("claude: %v %s", err, out)
		}
		return string(out)
	}

	out := run("用 Bash 工具執行 `touch allowed.txt`，只執行這一個指令，然後一句話回報結果。")
	t.Logf("allow 回覆：%s", strings.TrimSpace(out))
	if _, err := os.Stat(filepath.Join(tmp, "work", "allowed.txt")); err != nil {
		t.Fatalf("放行後檔案應存在：%v", err)
	}

	out = run("用 Bash 工具執行 `touch denied.txt`，只嘗試一次，不要換別的方法，然後一句話回報結果。")
	t.Logf("deny 回覆：%s", strings.TrimSpace(out))
	if _, err := os.Stat(filepath.Join(tmp, "work", "denied.txt")); err == nil {
		t.Fatal("拒絕後檔案不應存在")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0].Binding != "it" || seen[0].ToolName != "Bash" {
		t.Fatalf("broker 應收到兩次 Bash 請求：%+v", seen)
	}
}
