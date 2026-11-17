package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 「允許並記住」的分類鍵（與舊版相同）：
//
//	Bash：命中的危險樣式，例如 bash:npm install（一般指令本來就不問，不需要記）
//	其他工具：tool:<名稱>，例如 tool:WebFetch

// Keys 回傳這次工具呼叫的記住鍵；nil 表示這種呼叫不能記住。
// Bash 的鍵是命中的危險樣式（例如 bash:npm install、bash:curl），一條指令命中多個就要每個都記住過；
// 其他工具是 tool:<名稱>。
func Keys(tool string, input json.RawMessage) []string {
	if tool != "Bash" {
		return []string{"tool:" + tool}
	}
	var m struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &m)
	hits := RiskyPatterns(m.Command)
	if len(hits) == 0 {
		return nil
	}
	keys := make([]string, len(hits))
	for i, h := range hits {
		keys[i] = "bash:" + strings.TrimSpace(h)
	}
	sort.Strings(keys)
	return keys
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
