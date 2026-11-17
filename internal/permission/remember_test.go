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
		"npm install left-pad":                  {"bash:npm install"},
		"cd web && npm ci && curl -s https://x": {"bash:curl", "bash:npm ci"},
		"sudo apt install x":                    {"bash:apt", "bash:sudo"},
		"rm -rf build":                          {"bash:rm -rf"},
		"echo $(curl https://x)":                {"bash:curl"}, // 子字串比對，藏在 $( ) 裡也抓得到
	}
	for cmd, want := range cases {
		if got := Keys("Bash", bashIn(cmd)); !reflect.DeepEqual(got, want) {
			t.Errorf("%q → %v，預期 %v", cmd, got, want)
		}
	}
	for _, cmd := range []string{"git status", "go test ./...", "rtk ls -l", "  "} {
		if got := Keys("Bash", bashIn(cmd)); got != nil {
			t.Errorf("%q 不是危險指令，不該有記住鍵：%v", cmd, got)
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
