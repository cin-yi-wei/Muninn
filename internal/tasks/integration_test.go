//go:build integration

// 真的 claude 跑一個 readonly 委派任務（會消耗額度）：
//
//	go test -tags integration -run Integration -v ./internal/tasks/
package tasks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"muninn/internal/permission"
	"muninn/internal/runner"
	"muninn/internal/session"
)

func TestIntegrationReadonlyTask(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "muninn")
	if out, err := exec.Command("go", "build", "-o", exe, "muninn/cmd/muninn").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	repo := filepath.Join(tmp, "repo")
	_ = os.MkdirAll(repo, 0o755)
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("# 測試專案\n魔法數字是 4217。\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	socket := filepath.Join(tmp, "p.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var mu sync.Mutex
	var delivered string
	var denied []string
	m := &Manager{StateDir: filepath.Join(tmp, "tasks"), MaxRunning: 1, Logf: t.Logf}
	m.NewAsker = func(tk Task, extra []string) Asker {
		args := append([]string{"--mcp-config", permission.TaskMCPConfig(exe, socket, "task:"+tk.ID), "--strict-mcp-config",
			"--permission-prompt-tool", permission.PromptTool, "--model", "claude-haiku-5-5"}, extra...)
		return runner.New(runner.Config{Workdir: tk.Worktree, SessionID: tk.SessionID, ExtraArgs: args,
			SessionExists: func() bool { return session.Exists(filepath.Join(os.Getenv("HOME"), ".claude"), tk.SessionID) }})
	}
	m.Deliver = func(_, _, text string) error { mu.Lock(); delivered = text; mu.Unlock(); return nil }
	// 跟 daemon 相同的判斷；readonly 要問的一律拒絕
	go func() {
		_ = permission.ServeAll(ctx, socket, func(_ context.Context, req permission.Request) permission.Decision {
			id := strings.TrimPrefix(req.Binding, "task:")
			tk, _ := m.Lookup(id)
			if req.ToolName == "Bash" { // 與 daemon.taskPermission 相同：readonly 的 Bash 只放行唯讀指令
				var in struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal(req.Input, &in)
				if !permission.ReadonlyBash(in.Command) {
					mu.Lock()
					denied = append(denied, "Bash: "+in.Command)
					mu.Unlock()
					return permission.Deny("readonly 任務只能跑唯讀指令")
				}
				return permission.Allow(req.Input)
			}
			if ask, _ := permission.NeedsApproval(req.ToolName, req.Input, permission.Scope{Workdir: tk.Worktree, Repo: tk.Repo}); !ask {
				return permission.Allow(req.Input)
			}
			mu.Lock()
			denied = append(denied, req.ToolName)
			mu.Unlock()
			return permission.Deny("readonly 任務不能做這個操作")
		}, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tk, err := m.Delegate(Source{Binding: "chan", Repo: repo, Workdir: repo}, DelegateInput{
		Task: "讀 README.md，回報裡面寫的魔法數字。另外試著建立一個檔案 note.txt 寫入 hello（如果做不到就在報告裡說明）。",
		Role: "researcher",
	})
	if err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		mu.Lock()
		d := delivered
		mu.Unlock()
		if d != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("送回的結果：\n%s", delivered)
	t.Logf("被拒絕的工具：%v", denied)
	if !strings.Contains(delivered, "4217") {
		t.Fatal("報告裡應有魔法數字")
	}
	if out, _ := exec.Command("git", "-C", repo, "show", tk.Branch+":note.txt").CombinedOutput(); !strings.Contains(string(out), "does not exist") && !strings.Contains(string(out), "fatal") {
		t.Fatalf("readonly 任務不該寫出檔案：%s", out)
	}
}
