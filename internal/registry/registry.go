// Package registry 讀取 bindings 設定：頻道 ↔ repo + 分支 + worktree + 旗標。
//
// 設定檔是 JSON，密鑰（bot token）不放在檔案裡，只記環境變數名稱。範例見 muninn.example.json。
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Config 是整份設定檔。
type Config struct {
	StateDir     string   `json:"state_dir"`     // 各 binding 的佇列與 session 狀態放這底下，預設 ~/.local/state/muninn
	TokenEnv     string   `json:"token_env"`     // bot token 的環境變數名稱，預設 DISCORD_BOT_TOKEN
	AllowedUsers []string `json:"allowed_users"` // 預設白名單，binding 沒寫時套用
	ClaudeBin    string   `json:"claude_bin"`    // 選填
	// PermissionTimeout 是等使用者回 y/n 的上限（Go duration 字串），預設 30m
	PermissionTimeout string `json:"permission_timeout"`
	// IdleTimeout：claude 程序閒置多久就關掉（下次來訊息再 --resume），"0" 表示不回收，預設 30m
	IdleTimeout string `json:"idle_timeout"`
	// MemoryLimitMB：單一 binding 的 claude（含子程序）記憶體上限，超過就在閒置時重開；0 表示不檢查，預設 2048
	MemoryLimitMB *int `json:"memory_limit_mb"`
	// MaxAttempts：同一則訊息最多送進 claude 幾次（防崩潰無限回放），預設 3
	MaxAttempts int       `json:"max_attempts"`
	Bindings    []Binding `json:"bindings"`
}

// Binding 是一個頻道的綁定。
type Binding struct {
	Name         string   `json:"name"`          // 唯一名稱，也是狀態目錄名
	ChannelID    string   `json:"channel_id"`    // Discord 頻道 id
	Repo         string   `json:"repo"`          // 主 repo 路徑（建 worktree 用）
	Branch       string   `json:"branch"`        // 這個頻道工作的分支
	Base         string   `json:"base"`          // 分支不存在時從哪裡開，預設 HEAD
	Worktree     string   `json:"worktree"`      // worktree 路徑；空字串＝直接在 Repo 裡工作
	AllowedUsers []string `json:"allowed_users"` // 覆寫預設白名單
	AllowAnyone  bool     `json:"allow_anyone"`  // 白名單為空時必須明確設 true
	Model        string   `json:"model"`
	ClaudeArgs   []string `json:"claude_args"`
	QuoteReply   bool     `json:"quote_reply"`
	Paused       bool     `json:"paused"`       // true：不收這個頻道的訊息
	AutoApprove  bool     `json:"auto_approve"` // true：claude 的權限請求直接放行，不貼頻道確認
}

// Workdir 回傳 claude 實際的工作目錄。
func (b Binding) Workdir() string {
	if b.Worktree != "" {
		return b.Worktree
	}
	return b.Repo
}

// Allowed 回傳生效的白名單（binding 自己的優先）。
func (b Binding) Allowed(c Config) map[string]bool {
	src := b.AllowedUsers
	if len(src) == 0 {
		src = c.AllowedUsers
	}
	m := map[string]bool{}
	for _, u := range src {
		if u = strings.TrimSpace(u); u != "" {
			m[u] = true
		}
	}
	return m
}

// StateDirFor 回傳某個 binding 的狀態目錄。
func (c Config) StateDirFor(name string) string {
	return filepath.Join(c.StateDir, "bindings", name)
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Load 讀取並驗證設定檔，套用預設值，展開路徑開頭的 ~。
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields() // 欄位打錯字直接報錯，不要默默忽略
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("registry: 解析 %s: %w", path, err)
	}
	if c.StateDir == "" {
		c.StateDir = "~/.local/state/muninn"
	}
	if c.TokenEnv == "" {
		c.TokenEnv = "DISCORD_BOT_TOKEN"
	}
	if c.PermissionTimeout == "" {
		c.PermissionTimeout = "30m"
	}
	if c.IdleTimeout == "" {
		c.IdleTimeout = "30m"
	}
	if d, err := time.ParseDuration(c.IdleTimeout); err != nil || d < 0 {
		return Config{}, fmt.Errorf("registry: idle_timeout %q 不是合法的時間長度", c.IdleTimeout)
	}
	if c.MemoryLimitMB == nil {
		def := 2048
		c.MemoryLimitMB = &def
	}
	if *c.MemoryLimitMB < 0 {
		return Config{}, fmt.Errorf("registry: memory_limit_mb 不能是負數")
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if d, err := time.ParseDuration(c.PermissionTimeout); err != nil || d <= 0 {
		return Config{}, fmt.Errorf("registry: permission_timeout %q 不是合法的時間長度", c.PermissionTimeout)
	}
	c.StateDir = expand(c.StateDir)
	c.ClaudeBin = expand(c.ClaudeBin)

	names, channels := map[string]bool{}, map[string]bool{}
	for i := range c.Bindings {
		b := &c.Bindings[i]
		b.Repo, b.Worktree = expand(b.Repo), expand(b.Worktree)
		switch {
		case !nameRE.MatchString(b.Name):
			return Config{}, fmt.Errorf("registry: binding #%d 名稱 %q 不合法", i, b.Name)
		case names[b.Name]:
			return Config{}, fmt.Errorf("registry: binding 名稱 %q 重複", b.Name)
		case b.ChannelID == "":
			return Config{}, fmt.Errorf("registry: %s 缺 channel_id", b.Name)
		case channels[b.ChannelID]:
			return Config{}, fmt.Errorf("registry: 頻道 %s 綁了兩次", b.ChannelID)
		case b.Repo == "":
			return Config{}, fmt.Errorf("registry: %s 缺 repo", b.Name)
		case b.Worktree != "" && b.Branch == "":
			return Config{}, fmt.Errorf("registry: %s 有 worktree 就必須指定 branch", b.Name)
		case len(b.Allowed(c)) == 0 && !b.AllowAnyone:
			// 頻道裡任何人都能操控 claude 是危險的預設，要求明確表態
			return Config{}, fmt.Errorf("registry: %s 沒有白名單；真的要開放所有人請設 allow_anyone: true", b.Name)
		}
		names[b.Name], channels[b.ChannelID] = true, true
	}
	return c, nil
}

// Idle 回傳解析後的 idle_timeout（Load 已驗證過），0 表示不回收。
func (c Config) Idle() time.Duration {
	d, _ := time.ParseDuration(c.IdleTimeout)
	return d
}

// MemLimitBytes 回傳記憶體上限（bytes），0 表示不檢查。
func (c Config) MemLimitBytes() int64 {
	if c.MemoryLimitMB == nil {
		return 0
	}
	return int64(*c.MemoryLimitMB) << 20
}

// PermTimeout 回傳解析後的 permission_timeout（Load 已驗證過）。
func (c Config) PermTimeout() time.Duration {
	d, _ := time.ParseDuration(c.PermissionTimeout)
	return d
}

// Find 依名稱找 binding。
func (c Config) Find(name string) (Binding, bool) {
	for _, b := range c.Bindings {
		if b.Name == name {
			return b, true
		}
	}
	return Binding{}, false
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
