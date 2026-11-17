package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "muninn.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndInheritance(t *testing.T) {
	c, err := Load(write(t, `{
	  "allowed_users": ["u1"],
	  "bindings": [
	    {"name": "a", "channel_id": "c1", "repo": "~/r", "branch": "main", "worktree": "~/r-main"},
	    {"name": "b", "channel_id": "c2", "repo": "/r2", "allowed_users": ["u9"]}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if c.TokenEnv != "DISCORD_BOT_TOKEN" || c.StateDir != filepath.Join(home, ".local/state/muninn") {
		t.Fatalf("預設值不對：%+v", c)
	}
	a, _ := c.Find("a")
	if a.Workdir() != filepath.Join(home, "r-main") || a.Repo != filepath.Join(home, "r") {
		t.Fatalf("~ 沒展開：%+v", a)
	}
	if !a.Allowed(c)["u1"] {
		t.Fatal("a 應繼承預設白名單")
	}
	b, _ := c.Find("b")
	if b.Workdir() != "/r2" || b.Allowed(c)["u1"] || !b.Allowed(c)["u9"] {
		t.Fatalf("b 應用自己的白名單、直接在 repo 工作：%+v", b)
	}
	if c.StateDirFor("a") != filepath.Join(c.StateDir, "bindings", "a") {
		t.Fatal(c.StateDirFor("a"))
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"重複":            `{"allowed_users":["u"],"bindings":[{"name":"a","channel_id":"c1","repo":"/r"},{"name":"a","channel_id":"c2","repo":"/r"}]}`,
		"綁了兩次":          `{"allowed_users":["u"],"bindings":[{"name":"a","channel_id":"c1","repo":"/r"},{"name":"b","channel_id":"c1","repo":"/r"}]}`,
		"缺 repo":        `{"allowed_users":["u"],"bindings":[{"name":"a","channel_id":"c1"}]}`,
		"必須指定 branch":   `{"allowed_users":["u"],"bindings":[{"name":"a","channel_id":"c1","repo":"/r","worktree":"/w"}]}`,
		"沒有白名單":         `{"bindings":[{"name":"a","channel_id":"c1","repo":"/r"}]}`,
		"不合法":           `{"allowed_users":["u"],"bindings":[{"name":"../x","channel_id":"c1","repo":"/r"}]}`,
		"unknown field": `{"allowed_users":["u"],"bindingz":[]}`,
	}
	for want, body := range cases {
		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("預期錯誤含 %q，得到 %v", want, err)
		}
	}
	if _, err := Load(write(t, `{"bindings":[{"name":"a","channel_id":"c1","repo":"/r","allow_anyone":true}]}`)); err != nil {
		t.Fatalf("明確 allow_anyone 應允許：%v", err)
	}
}
