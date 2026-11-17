package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Muninn 自己決定哪些工具呼叫要問人（與舊版同一套規則）：
//
//   - Bash：只有命中危險樣式（安裝套件、下載、sudo、rm -rf…）才問，其他直接放行
//   - Edit / Write / MultiEdit / NotebookEdit：寫在工作目錄、binding 狀態目錄、session scratchpad、
//     專案記憶目錄裡直接放行；寫到其他地方才問
//   - WebFetch / WebSearch / MCP 工具：問
//   - 其他（Read、Grep、Glob、TodoWrite…唯讀或無副作用）：放行

// riskyPatterns 是需要確認的 Bash 樣式（小寫子字串比對，整條指令只要命中一個就問）。
var riskyPatterns = []string{
	"npm install", "npm i ", "npm ci", "yarn add", "pnpm add", "pnpm install",
	"pip install", "pip3 install", "gem install", "bundle install", "bundle add",
	"apt ", "apt-get", "apt-add", "dpkg", "brew install", "cargo install",
	"go install", "go get", "curl ", "wget ", "sudo ", "rm -rf", "rm -fr", "mkfs", "dd if=",
}

// RiskyPatterns 回傳指令命中的危險樣式（去重、照清單順序），沒命中回 nil。
func RiskyPatterns(cmd string) []string {
	c := strings.ToLower(cmd) + " " // 結尾補空白：讓 "sudo"、"curl" 單獨出現在結尾也命中
	var hits []string
	for _, p := range riskyPatterns {
		if strings.Contains(c, p) {
			hits = append(hits, p)
		}
	}
	return hits
}

// Scope 是一個 binding 可以自由寫入的目錄。
type Scope struct {
	Workdir  string // claude 的工作目錄（worktree）
	Repo     string // 主 repo（記憶目錄掛在這個 slug 上）
	StateDir string // binding 狀態目錄（附件等）
}

var nonAlnumRE = regexp.MustCompile(`[^a-zA-Z0-9]`)

func slug(p string) string { return nonAlnumRE.ReplaceAllString(clean(p), "-") }

func clean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// roots 回傳可以自由寫入的目錄。
func (s Scope) roots() []string {
	var r []string
	add := func(p string) {
		if p != "" {
			r = append(r, clean(p))
		}
	}
	add(s.Workdir)
	add(s.StateDir)
	if s.Workdir != "" {
		// Claude Code 給這個工作目錄的 scratchpad：<TMPDIR>/claude-<uid>/<工作目錄 slug>
		add(filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()), slug(s.Workdir)))
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{s.Repo, s.Workdir} {
			if p != "" {
				add(filepath.Join(home, ".claude", "projects", slug(p), "memory"))
			}
		}
	}
	return r
}

func (s Scope) contains(path string) bool {
	if path == "" {
		return false
	}
	p := clean(path)
	if !filepath.IsAbs(path) && s.Workdir != "" {
		p = clean(filepath.Join(s.Workdir, path))
	}
	for _, r := range s.roots() {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// NeedsApproval 判斷這次工具呼叫要不要問人；不用問時 reason 說明為什麼放行（記 log 用）。
func NeedsApproval(tool string, input json.RawMessage, scope Scope) (ask bool, reason string) {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	str := func(k string) string { v, _ := m[k].(string); return v }
	switch {
	case tool == "Bash":
		if hits := RiskyPatterns(str("command")); len(hits) > 0 {
			return true, ""
		}
		return false, "一般指令"
	case tool == "Edit" || tool == "Write" || tool == "MultiEdit" || tool == "NotebookEdit":
		path := str("file_path")
		if path == "" {
			path = str("notebook_path")
		}
		if scope.contains(path) {
			return false, "工作範圍內的寫入"
		}
		return true, ""
	case tool == "WebFetch" || tool == "WebSearch" || strings.HasPrefix(tool, "mcp__"):
		return true, ""
	}
	return false, "唯讀工具"
}
