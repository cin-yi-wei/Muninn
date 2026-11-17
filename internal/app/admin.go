package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"muninn/internal/admin"
	"muninn/internal/permission"
)

// AdminOps 是頻道管理能力（admin.Admin 實作）。只有控制頻道會設定。
type AdminOps interface {
	List() (string, error)
	Bind(ctx context.Context, o admin.BindOptions) (string, error)
	Unbind(ctx context.Context, o admin.UnbindOptions) (string, error)
	SetPaused(name string, paused bool) (string, error)
	Set(name, key, value string) (string, error)
}

const controlHelp = "**控制頻道指令**\n" +
	"`!list`：列出所有頻道\n" +
	"`!bind <名稱> <repo> [分支]`：新增（Discord 自動建同名頻道；加 `--channel=ID` 綁現有頻道、`--telegram=CHAT_ID` 綁 Telegram）\n" +
	"`!unbind <名稱> [--delete-channel] [--keep-worktree]`：移除（未提交的改動會先 wip commit）\n" +
	"`!pause <名稱>` / `!resume <名稱>`：暫停／恢復\n" +
	"`!set <名稱> <欄位>=<值>`：auto_approve、activity、quote_reply、control、model\n" +
	"也可以直接用講的，claude 會用管理工具幫你做"

// handleAdminCommand 處理控制頻道的管理指令；回傳 true 表示已處理。
func (a *App) handleAdminCommand(cmd string, args []string) bool {
	if a.Admin == nil {
		return false
	}
	run := func(f func() (string, error)) {
		go func() {
			msg, err := f()
			if err != nil {
				a.notify("⚠️ " + err.Error())
				return
			}
			a.notify(msg)
		}()
	}
	need := func(n int, usage string) bool {
		if len(args) < n {
			go a.notify("用法：" + usage)
			return false
		}
		return true
	}
	switch cmd {
	case "!list":
		run(a.Admin.List)
	case "!bind":
		if !need(2, "`!bind <名稱> <repo> [分支] [--channel=ID | --telegram=CHAT_ID] [--auto-approve]`") {
			return true
		}
		o := admin.BindOptions{Name: args[0], Repo: args[1], CreateChannel: true}
		for _, x := range args[2:] {
			switch {
			case strings.HasPrefix(x, "--channel="):
				o.ChannelID, o.CreateChannel = strings.TrimPrefix(x, "--channel="), false
			case strings.HasPrefix(x, "--telegram="):
				o.ChannelID, o.Platform, o.CreateChannel = strings.TrimPrefix(x, "--telegram="), "telegram", false
			case x == "--auto-approve":
				o.AutoApprove = true
			case strings.HasPrefix(x, "--base="):
				o.Base = strings.TrimPrefix(x, "--base=")
			case !strings.HasPrefix(x, "--") && o.Branch == "":
				o.Branch = x
			}
		}
		run(func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return a.Admin.Bind(ctx, o)
		})
	case "!unbind":
		if !need(1, "`!unbind <名稱> [--delete-channel] [--keep-worktree]`") {
			return true
		}
		o := admin.UnbindOptions{Name: args[0]}
		for _, x := range args[1:] {
			o.DeleteChannel = o.DeleteChannel || x == "--delete-channel"
			o.KeepWorktree = o.KeepWorktree || x == "--keep-worktree"
		}
		run(func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return a.Admin.Unbind(ctx, o)
		})
	case "!pause", "!resume":
		if !need(1, "`"+cmd+" <名稱>`") {
			return true
		}
		run(func() (string, error) { return a.Admin.SetPaused(args[0], cmd == "!pause") })
	case "!set":
		if !need(2, "`!set <名稱> <欄位>=<值>`") {
			return true
		}
		k, v, ok := strings.Cut(args[1], "=")
		if !ok {
			go a.notify("用法：`!set <名稱> <欄位>=<值>`")
			return true
		}
		run(func() (string, error) { return a.Admin.Set(args[0], k, v) })
	default:
		return false
	}
	return true
}

// adminTool 處理控制頻道 claude 的 binding_* 工具。
func (a *App) adminTool(method string, input json.RawMessage) permission.ToolResult {
	if a.Admin == nil {
		return toolErr("這不是控制頻道，沒有管理工具")
	}
	var in struct {
		Name          string `json:"name"`
		Repo          string `json:"repo"`
		Branch        string `json:"branch"`
		Base          string `json:"base"`
		Platform      string `json:"platform"`
		ChannelID     string `json:"channel_id"`
		AutoApprove   bool   `json:"auto_approve"`
		Model         string `json:"model"`
		DeleteChannel bool   `json:"delete_channel"`
		KeepWorktree  bool   `json:"keep_worktree"`
		Paused        bool   `json:"paused"`
		Key           string `json:"key"`
		Value         string `json:"value"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return toolErr("參數格式錯誤：%v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var msg string
	var err error
	switch method {
	case "binding_list":
		msg, err = a.Admin.List()
	case "binding_add":
		msg, err = a.Admin.Bind(ctx, admin.BindOptions{Name: in.Name, Repo: in.Repo, Branch: in.Branch, Base: in.Base,
			Platform: in.Platform, ChannelID: in.ChannelID, CreateChannel: in.ChannelID == "", AutoApprove: in.AutoApprove, Model: in.Model})
	case "binding_remove":
		msg, err = a.Admin.Unbind(ctx, admin.UnbindOptions{Name: in.Name, DeleteChannel: in.DeleteChannel, KeepWorktree: in.KeepWorktree})
	case "binding_pause":
		msg, err = a.Admin.SetPaused(in.Name, in.Paused)
	case "binding_set":
		msg, err = a.Admin.Set(in.Name, in.Key, in.Value)
	default:
		return toolErr("不認得的工具 %s", method)
	}
	if err != nil {
		return toolErr("%v", err)
	}
	if method != "binding_list" {
		a.logf("admin: %s %s", method, in.Name)
	}
	return permission.ToolResult{Text: msg}
}

// SelfPause 讓頻道自己暫停（!pause 不帶名稱時用）；暫停後只認 !resume。
type SelfPauser interface {
	SetPaused(name string, paused bool) (string, error)
}

func (a *App) selfPause() {
	if a.Pauser == nil || a.Name == "" {
		go a.notify("這個頻道不支援暫停")
		return
	}
	go func() {
		msg, err := a.Pauser.SetPaused(a.Name, true)
		if err != nil {
			a.notify("⚠️ " + err.Error())
			return
		}
		a.notify("⏸ " + msg)
	}()
}
