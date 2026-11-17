package permission

import (
	"encoding/json"
	"reflect"
	"testing"
)

func bashIn(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestKeys(t *testing.T) {
	cases := map[string][]string{
		"git status --short":                  {"bash:git status"},
		"git push -f origin main":             {"bash:git push"},
		"ls -la && rm -rf build":              {"bash:ls", "bash:rm"},
		"FOO=1 go test ./... | tail -5":       {"bash:go test", "bash:tail"},
		"sudo apt install x":                  {"bash:sudo"},
		"/usr/bin/python3 -I x.py; echo done": {"bash:echo", "bash:python3"},
		"git -C /repo log":                    {"bash:git"},
		"rtk git status":                      {"bash:git status"},
		"rtk git push origin main":            {"bash:git push"},
		"rtk proxy go test ./...":             {"bash:go test"},
		"rtk ls -l && rtk git diff":           {"bash:git diff", "bash:ls"},
	}
	for cmd, want := range cases {
		if got := Keys("Bash", bashIn(cmd)); !reflect.DeepEqual(got, want) {
			t.Errorf("%q → %v，預期 %v", cmd, got, want)
		}
	}
	for _, cmd := range []string{"echo $(whoami)", "echo `id`", "  "} {
		if got := Keys("Bash", bashIn(cmd)); got != nil {
			t.Errorf("%q 不該能記住：%v", cmd, got)
		}
	}
	if got := Keys("Write", json.RawMessage(`{"file_path":"/a"}`)); !reflect.DeepEqual(got, []string{"tool:Write"}) {
		t.Errorf("Write → %v", got)
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if s.Covers([]string{"bash:ls"}) || s.Covers(nil) {
		t.Fatal("空的 store 不該涵蓋任何東西")
	}
	if err := s.Add([]string{"bash:ls", "bash:git status"}); err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(dir) // 重新讀檔，確認有持久化
	if !s2.Covers([]string{"bash:ls"}) || !s2.Covers([]string{"bash:ls", "bash:git status"}) {
		t.Fatal("應涵蓋已記住的鍵")
	}
	if s2.Covers([]string{"bash:ls", "bash:rm"}) {
		t.Fatal("只要有一段沒記住就不該涵蓋")
	}
}
