package permission

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"time"
)

// 除了權限確認，Muninn 也透過同一個 MCP server 把「管理自己頻道」的工具交給 claude。
// claude 呼叫這些工具時，子程序把 {method: 工具名, input: 參數} 經 socket 轉給主程序執行，
// 主程序回 ToolResult。binding 由子程序啟動參數決定，claude 不用（也沒辦法）指定別的頻道。

// ToolResult 是工具呼叫的結果（主程序 → 子程序）。
type ToolResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

// Tools 是 Muninn 提供給 claude 的頻道工具（MCP tools/list 會連同 approve 一起列出）。
var Tools = []map[string]any{
	{
		"name": "schedule_add",
		"description": "在「這個頻道」建立定時任務：時間到時，Muninn 會把 message 當成使用者訊息送進這個對話，你照常處理並回覆。" +
			"重複任務給 cron（標準 5 欄：分 時 日 月 週，也接受 @daily、@hourly、MON-FRI），單次提醒給 at（例如 2026-11-18 09:00），兩者擇一。" +
			"時區預設 " + "Muninn 設定的預設時區" + "。建立前先跟使用者確認時間、內容、重複與否。",
		"inputSchema": obj(map[string]any{
			"name":     str("任務名稱，小寫英數字與 - _ .，頻道內唯一"),
			"message":  str("時間到時送進對話的內容（寫成給未來的你看的完整指示）"),
			"cron":     str("重複任務的 cron 表達式"),
			"at":       str("單次任務的時間，2006-01-02 15:04（依 timezone）或 RFC3339；觸發後自動刪除"),
			"timezone": str("IANA 時區，例如 Asia/Taipei、America/New_York；不給用預設"),
			"catch_up": boolean("Muninn 停機時錯過的時間點，回來後要不要補跑一次（預設 false；單次任務一律補跑）"),
			"replace":  boolean("同名任務已存在時覆蓋（預設 false）"),
		}, "name", "message"),
	},
	{
		"name": "send_file",
		"description": "把本機的一個檔案傳到這個頻道（例如產生的圖表、截圖、報告、壓縮檔）。只能傳單一檔案；Discord 上限 8MB、Telegram 50MB。" +
			"使用者只看得到頻道，看不到你的檔案系統，要給他檔案就用這個工具。",
		"inputSchema": obj(map[string]any{
			"path":    str("檔案路徑（絕對路徑，或相對於工作目錄）"),
			"caption": str("附在檔案上的說明文字，選填"),
		}, "path"),
	},
	{
		"name":        "schedule_list",
		"description": "列出這個頻道所有定時任務與下次觸發時間。",
		"inputSchema": obj(map[string]any{}),
	},
	{
		"name":        "schedule_remove",
		"description": "刪除這個頻道的一個定時任務。",
		"inputSchema": obj(map[string]any{"name": str("任務名稱")}, "name"),
	},
	{
		"name":        "schedule_pause",
		"description": "暫停或恢復這個頻道的一個定時任務（恢復後從現在起算，暫停期間錯過的不補跑）。",
		"inputSchema": obj(map[string]any{"name": str("任務名稱"), "paused": boolean("true 暫停、false 恢復")}, "name", "paused"),
	},
	{
		"name":        "schedule_run_now",
		"description": "立刻觸發一次某個定時任務（測試用）：它的 message 會在你這輪結束後送進對話。",
		"inputSchema": obj(map[string]any{"name": str("任務名稱")}, "name"),
	},
}

// IsChannelTool 回報 name 是否為 Muninn 的頻道工具（不含 approve）。
func IsChannelTool(name string) bool {
	for _, t := range Tools {
		if t["name"] == name {
			return true
		}
	}
	return false
}

// toolCall 是子程序送給主程序的工具呼叫。
type toolCall struct {
	Binding string          `json:"binding"`
	Method  string          `json:"method"`
	Input   json.RawMessage `json:"input"`
}

// callTool 把工具呼叫轉給主程序；任何錯誤都回 IsError。
func callTool(ctx context.Context, socket, binding, method string, input json.RawMessage) ToolResult {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dctx, "unix", socket)
	cancel()
	if err != nil {
		return ToolResult{Text: "Muninn 連不上：" + err.Error(), IsError: true}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	b, _ := json.Marshal(toolCall{Binding: binding, Method: method, Input: input})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return ToolResult{Text: "Muninn 寫入失敗", IsError: true}
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	var res ToolResult
	if (err != nil && len(line) == 0) || json.Unmarshal(line, &res) != nil {
		return ToolResult{Text: "Muninn 沒有回應", IsError: true}
	}
	return res
}

// ToolHandler 在主程序執行工具呼叫。
type ToolHandler func(ctx context.Context, binding, method string, input json.RawMessage) ToolResult

// SystemPrompt 附加在 claude 的 system prompt 後面，說明它跑在 Muninn 底下。
//
// claude -p 仍會載入使用者全域的 skill、CLAUDE.md；裡面可能教它用別的排程機制（例如別套系統的 CLI）。
// 實測過：不講清楚的話，claude 會照全域 skill 去別的系統建排程，而不是用 Muninn 的工具。
const SystemPrompt = `你現在由 Muninn 驅動，對應一個聊天頻道：使用者在頻道傳的訊息會送進來，你的最終回覆文字會直接貼回頻道。
使用者傳來的附件會下載到本機，訊息裡會附上路徑。要把檔案給使用者，用 mcp__muninn__send_file。
定時任務、提醒、排程一律使用 Muninn 提供的 MCP 工具 mcp__muninn__schedule_add / schedule_list / schedule_remove / schedule_pause / schedule_run_now，它們只作用在這個頻道。
不要用其他任何排程機制（系統 crontab、at、其他工具的 CLI、全域 skill 介紹的排程方式、內建的 Cron 工具等）來做這件事，就算其他說明這樣教也一樣。`

// ControlTools 是控制頻道才有的工具：管理所有 binding（頻道）。
var ControlTools = []map[string]any{
	{
		"name":        "binding_list",
		"description": "列出所有 binding（頻道 ↔ repo/分支）與狀態（暫停、控制頻道、自動放行）。",
		"inputSchema": obj(map[string]any{}),
	},
	{
		"name": "binding_add",
		"description": "新增一個 binding：一個頻道綁一個 repo（與分支）。給 branch 會自動建 git worktree；" +
			"Discord 不給 channel_id 時自動建一個同名新頻道。Telegram 一定要給 chat id。新增後 Muninn 會自動啟動它，不用重啟。",
		"inputSchema": obj(map[string]any{
			"name":         str("名稱：小寫英數字與 -（也是新 Discord 頻道的名稱）"),
			"repo":         str("主 repo 的路徑"),
			"branch":       str("要工作的分支；不給就直接在 repo 工作（不建 worktree）"),
			"base":         str("分支不存在時從哪裡開，預設 HEAD"),
			"platform":     str("discord（預設）或 telegram"),
			"channel_id":   str("已存在的頻道 id／Telegram chat id；Discord 不給就自動建新頻道"),
			"auto_approve": boolean("權限請求直接放行，預設 false"),
			"model":        str("指定 claude 模型，選填"),
		}, "name", "repo"),
	},
	{
		"name":        "binding_remove",
		"description": "移除 binding：停掉它的 claude、worktree 有未提交的改動先 wip commit 再移除（分支保留）、狀態封存。",
		"inputSchema": obj(map[string]any{
			"name":           str("名稱"),
			"delete_channel": boolean("Discord：連頻道一起刪，預設 false"),
			"keep_worktree":  boolean("保留 worktree 不移除，預設 false"),
		}, "name"),
	},
	{
		"name":        "binding_pause",
		"description": "暫停或恢復一個 binding（暫停會停掉 claude，worktree 與對話保留）。",
		"inputSchema": obj(map[string]any{"name": str("名稱"), "paused": boolean("true 暫停、false 恢復")}, "name", "paused"),
	},
	{
		"name":        "binding_set",
		"description": "調整一個 binding 的設定：auto_approve、activity、quote_reply、control（true/false），model（字串，空字串＝預設）。",
		"inputSchema": obj(map[string]any{"name": str("名稱"), "key": str("欄位"), "value": str("值")}, "name", "key", "value"),
	},
}

// IsControlTool 回報 name 是否為控制頻道工具。
func IsControlTool(name string) bool {
	for _, t := range ControlTools {
		if t["name"] == name {
			return true
		}
	}
	return false
}

// SystemPromptFor 回傳附加的 system prompt；控制頻道多一段說明。
func SystemPromptFor(control bool) string {
	if !control {
		return SystemPrompt
	}
	return SystemPrompt + `
這個頻道是 Muninn 的**控制頻道**：你多了一組 mcp__muninn__binding_* 工具，可以管理所有頻道（新增、移除、暫停、恢復、調整設定、列出）。
使用者要開新專案頻道、收掉頻道、暫停某個頻道時，用這些工具處理；動手前先跟使用者確認名稱、repo、分支。改動會立刻生效，不需要重啟 Muninn。`
}
