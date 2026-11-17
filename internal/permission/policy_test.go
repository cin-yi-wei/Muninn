package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRiskyPatterns(t *testing.T) {
	for cmd, risky := range map[string]bool{
		"go test ./...": false, "git push origin main": false, "ls -la": false, "rtk git status": false,
		"npm install": true, "pip3 install x": true, "curl -s https://a": true, "wget x": true,
		"sudo systemctl restart x": true, "rm -rf /tmp/x": true, "rm -fr x": true, "go get x": true,
		"echo hi | sudo": true, "SUDO ls": true,
	} {
		if got := len(RiskyPatterns(cmd)) > 0; got != risky {
			t.Errorf("%q：risky=%v，預期 %v", cmd, got, risky)
		}
	}
}

func TestNeedsApproval(t *testing.T) {
	home, _ := os.UserHomeDir()
	wt, state, repo := t.TempDir(), t.TempDir(), "/repo/main"
	sc := Scope{Workdir: wt, Repo: repo, StateDir: state}
	in := func(k, v string) json.RawMessage { b, _ := json.Marshal(map[string]string{k: v}); return b }
	scratch := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()), slug(wt), "x.md")
	cases := []struct {
		tool  string
		input json.RawMessage
		ask   bool
	}{
		{"Bash", in("command", "go test ./..."), false},
		{"Bash", in("command", "npm install"), true},
		{"Edit", in("file_path", filepath.Join(wt, "a.go")), false},
		{"Write", in("file_path", "rel/b.go"), false}, // 相對路徑以工作目錄為準
		{"Write", in("file_path", filepath.Join(state, "attachments", "x")), false},
		{"Write", in("file_path", scratch), false},
		{"Write", in("file_path", filepath.Join(home, ".claude", "projects", slug(repo), "memory", "m.md")), false},
		{"Write", in("file_path", filepath.Join(home, ".claude", "projects", "-other", "memory", "m.md")), true},
		{"Write", in("file_path", wt+"-evil/x"), true}, // 前綴相同但不是子目錄
		{"Edit", in("file_path", "/etc/hosts"), true},
		{"Write", in("file_path", filepath.Join(wt, "..", "escape")), true},
		{"WebFetch", in("url", "https://a"), true},
		{"mcp__muninn__schedule_add", in("name", "x"), true},
		{"Read", in("file_path", "/etc/passwd"), false},
		{"Grep", in("pattern", "x"), false},
	}
	for _, c := range cases {
		if ask, _ := NeedsApproval(c.tool, c.input, sc); ask != c.ask {
			t.Errorf("%s %s：ask=%v，預期 %v", c.tool, c.input, ask, c.ask)
		}
	}
}
