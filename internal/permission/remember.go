package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// 「允許並記住」的分類鍵：
//
//	Bash：每一段指令取「指令名」，常見有子指令的工具再加上子指令，例如 bash:git push、bash:go test、bash:rm
//	其他工具：tool:<名稱>，例如 tool:Write
//
// 一個 Bash 指令用 && || ; | 串了好幾段時，會得到多個鍵；**每一段都記住過才自動放行**，
// 免得記住了 `ls` 之後 `ls && rm -rf /` 也跟著被放行。
// 含 $( ) 或反引號的指令無法可靠拆解，一律不給記住（Keys 回 nil）。

// 這些工具的第二個字是子指令，記住時要連子指令一起（記住 git status 不等於記住 git push）。
var subcommandTools = map[string]bool{
	"git": true, "go": true, "npm": true, "pnpm": true, "yarn": true, "cargo": true, "docker": true,
	"kubectl": true, "gh": true, "pip": true, "pip3": true, "bundle": true, "rails": true, "make": true,
	"systemctl": true, "apt": true, "apt-get": true, "brew": true,
}

// 這些是包裝指令，本身不算分類，要跳過看底下真正的指令。
// rtk：使用者全域掛了 rtk hook，claude 的 `git status` 會被改寫成 `rtk git status` 才送來確認，
// 不跳過的話會記成 `bash:rtk git`，變成連 git push 都一起放行。
var wrappers = map[string]bool{"env": true, "time": true, "nice": true, "nohup": true, "rtk": true}

var segmentSplit = regexp.MustCompile(`&&|\|\||;|\||\n`)

// Keys 回傳這次工具呼叫的記住鍵；nil 表示這種呼叫不能記住。
func Keys(tool string, input json.RawMessage) []string {
	if tool != "Bash" {
		return []string{"tool:" + tool}
	}
	var m struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &m)
	cmd := m.Command
	if strings.TrimSpace(cmd) == "" || strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") {
		return nil
	}
	seen := map[string]bool{}
	var keys []string
	for _, seg := range segmentSplit.Split(cmd, -1) {
		k := segmentKey(seg)
		if k == "" {
			continue
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return keys
}

// segmentKey 取出一段指令的分類鍵：跳過 VAR=value 前綴、sudo / env 之類的包裝。
func segmentKey(seg string) string {
	f := strings.Fields(seg)
	i := 0
	for i < len(f) && (strings.Contains(f[i], "=") && !strings.HasPrefix(f[i], "-") || wrappers[filepath.Base(f[i])] || f[i] == "(" || f[i] == "{") {
		i++
		// rtk proxy <cmd> 也是包裝
		if i < len(f) && f[i-1] == "rtk" && f[i] == "proxy" {
			i++
		}
	}
	if i >= len(f) {
		return ""
	}
	name := filepath.Base(strings.TrimLeft(f[i], "({"))
	if name == "sudo" {
		// sudo 本身就是一類，不要被底下的指令名稀釋
		return "bash:sudo"
	}
	if subcommandTools[name] && i+1 < len(f) && !strings.HasPrefix(f[i+1], "-") {
		return "bash:" + name + " " + f[i+1]
	}
	return "bash:" + name
}

// Store 是一個 binding 記住的分類，存在 <stateDir>/permissions.json。
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore 建立 store；檔案不存在等同什麼都沒記住。
func NewStore(stateDir string) *Store {
	return &Store{path: filepath.Join(stateDir, "permissions.json")}
}

func (s *Store) load() map[string]bool {
	m := map[string]bool{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return m
	}
	var keys []string
	if json.Unmarshal(data, &keys) == nil {
		for _, k := range keys {
			m[k] = true
		}
	}
	return m
}

// Covers 回報 keys 是否全部記住過（keys 為空一律 false）。
func (s *Store) Covers(keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	for _, k := range keys {
		if !m[k] {
			return false
		}
	}
	return true
}

// Add 記住 keys。
func (s *Store) Add(keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	for _, k := range keys {
		m[k] = true
	}
	all := make([]string, 0, len(m))
	for k := range m {
		all = append(all, k)
	}
	sort.Strings(all)
	data, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
