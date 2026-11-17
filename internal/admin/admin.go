// Package admin 是頻道（binding）管理操作：新增、移除、暫停、恢復、調整設定、列出。
//
// CLI（muninn bind …）、聊天指令（控制頻道的 !bind …）、控制頻道 claude 的 MCP 工具都呼叫這裡。
// 所有操作只改設定檔，正在跑的 Muninn 會偵測到檔案變動自動套用（熱重載），不需要重啟。
package admin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"muninn/internal/audit"
	"muninn/internal/registry"
	"muninn/internal/session"
	"muninn/internal/worktree"
)

// ChannelMaker 建立／刪除 Discord 頻道（只有 Discord 需要）。
type ChannelMaker interface {
	CreateChannel(ctx context.Context, guildID, name, parentID, topic string) (string, error)
	DeleteChannel(ctx context.Context, channelID string) error
}

// Admin 綁一份設定檔。
type Admin struct {
	ConfigPath string
	Discord    ChannelMaker // 選填：沒有就不能自動建／刪 Discord 頻道
	Audit      *audit.Log   // 選填
	Actor      string       // 稽核 log 裡的操作者（cli、channel、web…）
}

// As 回傳一份操作者不同的複本（共用設定與稽核）。
func (a *Admin) As(actor string) *Admin {
	cp := *a
	cp.Actor = actor
	return &cp
}

func (a *Admin) record(action, binding, detail string, err error) {
	res := "ok"
	if err != nil {
		res, detail = "error", detail+"："+err.Error()
	}
	a.Audit.Record(audit.Event{Kind: "admin", Binding: binding, Actor: a.Actor, Action: action, Detail: detail, Result: res})
}

// BindOptions 是新增 binding 的參數。
type BindOptions struct {
	Name          string
	Platform      string // discord（預設）／telegram
	ChannelID     string // 已存在的頻道 id；空且 CreateChannel 為 true 時自動建
	CreateChannel bool   // Discord：建一個新頻道（名稱同 Name）
	Repo          string
	Branch        string // 空字串：直接在 repo 工作（不建 worktree）
	Base          string
	Worktree      string // 空字串且有 Branch：<worktree_root>/<name>
	AutoApprove   bool
	Control       bool
	Model         string
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,48}$`)

// Bind 新增一個 binding；回傳說明文字。
func (a *Admin) Bind(ctx context.Context, o BindOptions) (msg string, err error) {
	defer func() { a.record("bind", o.Name, o.Repo+" "+o.Branch, err) }()
	cfg, err := registry.Load(a.ConfigPath)
	if err != nil {
		return "", err
	}
	if !nameRE.MatchString(o.Name) {
		return "", fmt.Errorf("名稱 %q 不合法：小寫英數字與 -，最多 49 字（也會拿來當頻道名稱）", o.Name)
	}
	if _, ok := cfg.Find(o.Name); ok {
		return "", fmt.Errorf("binding %q 已存在", o.Name)
	}
	if o.Platform == "" {
		o.Platform = registry.PlatformDiscord
	}
	if o.Repo == "" {
		return "", fmt.Errorf("要給 repo")
	}
	if strings.HasPrefix(o.Repo, "~/") {
		home, _ := os.UserHomeDir()
		o.Repo = filepath.Join(home, o.Repo[2:])
	}
	if o.Branch != "" && o.Worktree == "" {
		o.Worktree = filepath.Join(cfg.WorktreeRoot, o.Name)
	}
	// 先確認 worktree 建得起來，再動頻道與設定，失敗時不留半套
	created, err := worktree.Ensure(o.Repo, o.Branch, o.Base, o.Worktree)
	if err != nil {
		return "", err
	}
	rollback := func() {
		if created {
			_, _ = worktree.Remove(o.Repo, o.Worktree)
		}
	}
	var notes []string
	if created {
		notes = append(notes, fmt.Sprintf("建立 worktree %s（分支 %s）", o.Worktree, o.Branch))
	}
	madeChannel := false
	if o.ChannelID == "" {
		if !o.CreateChannel || o.Platform != registry.PlatformDiscord {
			rollback()
			return "", fmt.Errorf("要給 channel id（Telegram 一定要給；Discord 可改用自動建頻道）")
		}
		if a.Discord == nil || cfg.DiscordGuild == "" {
			rollback()
			return "", fmt.Errorf("自動建 Discord 頻道需要 bot token 與設定檔的 discord_guild")
		}
		id, err := a.Discord.CreateChannel(ctx, cfg.DiscordGuild, o.Name, cfg.DiscordCategory, "Muninn："+o.Repo+" "+o.Branch)
		if err != nil {
			rollback()
			return "", fmt.Errorf("建 Discord 頻道失敗：%w", err)
		}
		o.ChannelID, madeChannel = id, true
		notes = append(notes, fmt.Sprintf("建立 Discord 頻道 <#%s>", id))
	}
	raw := registry.RawBinding{"name": o.Name, "channel_id": o.ChannelID, "repo": o.Repo}
	if o.Platform != registry.PlatformDiscord {
		raw["platform"] = o.Platform
	}
	if o.Branch != "" {
		raw["branch"] = o.Branch
	}
	if o.Base != "" {
		raw["base"] = o.Base
	}
	if o.Worktree != "" {
		raw["worktree"] = o.Worktree
	}
	if o.AutoApprove {
		raw["auto_approve"] = true
	}
	if o.Control {
		raw["control"] = true
	}
	if o.Model != "" {
		raw["model"] = o.Model
	}
	err = registry.Edit(a.ConfigPath, func(bs []registry.RawBinding) ([]registry.RawBinding, error) {
		return append(bs, raw), nil
	})
	if err != nil {
		if madeChannel {
			_ = a.Discord.DeleteChannel(ctx, o.ChannelID)
		}
		rollback()
		return "", err
	}
	notes = append(notes, fmt.Sprintf("新增 binding %s（%s 頻道 %s）", o.Name, o.Platform, o.ChannelID))
	return strings.Join(notes, "\n"), nil
}

// UnbindOptions 是移除 binding 的參數。
type UnbindOptions struct {
	Name          string
	DeleteChannel bool // Discord：連頻道一起刪
	KeepWorktree  bool
}

// Unbind 移除 binding：從設定拿掉（正在跑的 Muninn 會停掉它）、worktree 有改動先 wip commit 再移除、
// 狀態目錄封存到 <state_dir>/archive/。分支保留。
func (a *Admin) Unbind(ctx context.Context, o UnbindOptions) (msg string, err error) {
	defer func() {
		a.record("unbind", o.Name, fmt.Sprintf("delete_channel=%v keep_worktree=%v", o.DeleteChannel, o.KeepWorktree), err)
	}()
	cfg, err := registry.Load(a.ConfigPath)
	if err != nil {
		return "", err
	}
	b, ok := cfg.Find(o.Name)
	if !ok {
		return "", fmt.Errorf("找不到 binding %q", o.Name)
	}
	if err := registry.Edit(a.ConfigPath, func(bs []registry.RawBinding) ([]registry.RawBinding, error) {
		i := registry.FindRaw(bs, o.Name)
		return append(bs[:i], bs[i+1:]...), nil
	}); err != nil {
		return "", err
	}
	notes := []string{"已從設定移除 " + o.Name}
	// 等正在跑的 Muninn 停掉這個 binding 的 claude（熱重載每 3 秒檢查一次），再動 worktree
	waitStopped(cfg.StateDirFor(o.Name), 15*time.Second)
	if b.Worktree != "" && !o.KeepWorktree {
		wip, err := worktree.Remove(b.Repo, b.Worktree)
		switch {
		case err != nil:
			notes = append(notes, "⚠️ worktree 沒移除："+err.Error())
		case wip != "":
			notes = append(notes, fmt.Sprintf("worktree 有未提交的改動，已在分支 %s 上做 wip commit %s 後移除", b.Branch, wip))
		default:
			notes = append(notes, "已移除 worktree "+b.Worktree+"（分支 "+b.Branch+" 保留）")
		}
	}
	if dir := cfg.StateDirFor(o.Name); exists(dir) {
		arch := filepath.Join(cfg.StateDir, "archive", o.Name+"-"+time.Now().UTC().Format("20060102T150405Z"))
		if err := os.MkdirAll(filepath.Dir(arch), 0o755); err == nil && os.Rename(dir, arch) == nil {
			notes = append(notes, "狀態目錄封存到 "+arch+"（含 session id，之後要接回可用）")
		}
	}
	if o.DeleteChannel && b.Platform == registry.PlatformDiscord {
		if a.Discord == nil {
			notes = append(notes, "⚠️ 沒有 Discord token，頻道沒刪")
		} else if err := a.Discord.DeleteChannel(ctx, b.ChannelID); err != nil {
			notes = append(notes, "⚠️ 刪 Discord 頻道失敗："+err.Error())
		} else {
			notes = append(notes, "已刪除 Discord 頻道 "+b.ChannelID)
		}
	}
	return strings.Join(notes, "\n"), nil
}

// waitStopped 等 runner 不再持有 session（用 session 鎖檔判斷，見 session.Running）。
func waitStopped(stateDir string, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) && session.Running(stateDir) {
		time.Sleep(300 * time.Millisecond)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// SetPaused 暫停或恢復。
func (a *Admin) SetPaused(name string, paused bool) (string, error) {
	err := a.setField(name, "paused", paused)
	a.record(map[bool]string{true: "pause", false: "resume"}[paused], name, "", err)
	if err != nil {
		return "", err
	}
	if paused {
		return "已暫停 " + name + "（claude 停掉，worktree 與對話保留；在那個頻道打 !resume 或用 resume 指令恢復）", nil
	}
	return "已恢復 " + name + "（下一則訊息會接回原本的對話）", nil
}

// settable 是可以用 Set 調整的欄位與型別。
var settable = map[string]string{
	"auto_approve": "bool", "activity": "bool", "quote_reply": "bool", "control": "bool",
	"model": "string",
}

// Set 調整一個 binding 欄位，例如 auto_approve=true、model=claude-sonnet-5-5。
func (a *Admin) Set(name, key, value string) (string, error) {
	typ, ok := settable[key]
	if !ok {
		keys := make([]string, 0, len(settable))
		for k := range settable {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "", fmt.Errorf("不能調整 %q（可調整：%s）", key, strings.Join(keys, "、"))
	}
	var v any = value
	if typ == "bool" {
		bv, err := strconv.ParseBool(value)
		if err != nil {
			return "", fmt.Errorf("%s 要給 true 或 false", key)
		}
		v = bv
	}
	if typ == "string" && value == "" {
		v = nil // 清掉，回到預設
	}
	err := a.setField(name, key, v)
	a.record("set", name, key+"="+value, err)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s 的 %s 已設為 %s（會重啟這個頻道的 claude，對話保留）", name, key, value), nil
}

func (a *Admin) setField(name, key string, v any) error {
	return registry.Edit(a.ConfigPath, func(bs []registry.RawBinding) ([]registry.RawBinding, error) {
		i := registry.FindRaw(bs, name)
		if i < 0 {
			return nil, fmt.Errorf("找不到 binding %q", name)
		}
		if v == nil || v == false {
			delete(bs[i], key)
		} else {
			bs[i][key] = v
		}
		return bs, nil
	})
}

// List 回傳所有 binding 的摘要。
func (a *Admin) List() (string, error) {
	cfg, err := registry.Load(a.ConfigPath)
	if err != nil {
		return "", err
	}
	if len(cfg.Bindings) == 0 {
		return "沒有任何 binding", nil
	}
	var lines []string
	for _, b := range cfg.Bindings {
		var flags []string
		if b.Paused {
			flags = append(flags, "⏸ 暫停")
		}
		if b.Control {
			flags = append(flags, "🎛 控制")
		}
		if b.AutoApprove {
			flags = append(flags, "自動放行")
		}
		where := b.Repo
		if b.Branch != "" {
			where += "@" + b.Branch
		}
		ch := b.ChannelID
		if b.Platform == registry.PlatformDiscord {
			ch = "<#" + b.ChannelID + ">"
		}
		line := fmt.Sprintf("• `%s` %s %s　%s", b.Name, b.Platform, ch, where)
		if len(flags) > 0 {
			line += "　" + strings.Join(flags, "、")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}
