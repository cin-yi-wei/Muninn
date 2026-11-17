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

// readonlyCommands 是 readonly 任務可以跑的指令（第一個字）。不在清單上的一律拒絕。
var readonlyCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "grep": true, "rg": true, "find": true,
	"tree": true, "file": true, "stat": true, "du": true, "pwd": true, "echo": true, "printf": true, "which": true,
	"sort": true, "uniq": true, "cut": true, "diff": true, "jq": true, "awk": true, "basename": true, "dirname": true,
	"realpath": true, "date": true, "true": true, "test": true, "[": true, "cd": true, "sha256sum": true, "md5sum": true,
	// rtk 的唯讀子指令（使用者全域的 rtk hook 會把 cat 改寫成 rtk read 之類）
	"read": true, "json": true, "log": true,
}

// readonlyGit 是 readonly 任務可以跑的 git 子指令。
var readonlyGit = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "blame": true, "branch": true, "rev-parse": true,
	"ls-files": true, "grep": true, "describe": true, "shortlog": true, "cat-file": true, "ls-tree": true,
}

var segSplit = regexp.MustCompile(`&&|\|\||;|\||\n`)

// ReadonlyBash 判斷一條 Bash 指令是否只讀：每一段都在唯讀清單上，而且沒有輸出導向、
// 子指令替換（$( )、反引號）、sed -i 這類就地修改。判斷不了的一律當成不是唯讀。
func ReadonlyBash(cmd string) bool {
	if strings.TrimSpace(cmd) == "" || strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") {
		return false
	}
	// 輸出導向：> 或 >>（允許 2>&1、>/dev/null 這類丟掉輸出）
	cleaned := strings.NewReplacer("2>&1", "", ">/dev/null", "", "> /dev/null", "", "2>/dev/null", "", "&>/dev/null", "").Replace(cmd)
	if strings.Contains(cleaned, ">") {
		return false
	}
	for _, seg := range segSplit.Split(cmd, -1) {
		f := strings.Fields(seg)
		i := 0
		for i < len(f) && (f[i] == "rtk" || f[i] == "env" || f[i] == "(" || strings.Contains(f[i], "=") && !strings.HasPrefix(f[i], "-")) {
			i++
		}
		if i >= len(f) {
			continue
		}
		name := filepath.Base(f[i])
		switch {
		case name == "git":
			sub := ""
			for _, x := range f[i+1:] {
				if !strings.HasPrefix(x, "-") {
					sub = x
					break
				}
			}
			if !readonlyGit[sub] {
				return false
			}
		case name == "find" && (strings.Contains(seg, "-delete") || strings.Contains(seg, "-exec")):
			return false
		case name == "sed" && !strings.Contains(seg, " -i"):
			// sed 不帶 -i 只是輸出
		case !readonlyCommands[name]:
			return false
		}
	}
	return true
}
