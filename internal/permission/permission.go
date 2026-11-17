// Package permission 實作 claude 的 --permission-prompt-tool（DESIGN §3.4、M3）。
//
// 流程：
//
//	claude ──stdio MCP──▶ muninn permission-mcp（子程序）──unix socket──▶ muninn run（主程序 Broker）
//	                                                                         └─ 貼到頻道等 y/n，或 auto_approve 直接放行
//
// 子程序只負責 MCP 協定轉接；真正的決策在主程序，因為只有主程序握著 Discord 連線和頻道狀態。
package permission

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
)

// ServerName / ToolName 組成 claude 端看到的工具名稱 mcp__muninn__approve。
const (
	ServerName = "muninn"
	ToolName   = "approve"
	PromptTool = "mcp__" + ServerName + "__" + ToolName
)

// Request 是 claude 要求確認的一次工具呼叫（MCP 子程序 → 主程序）。
type Request struct {
	Binding   string          `json:"binding"`
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

// Decision 是回給 claude 的決策，格式即 --permission-prompt-tool 要求的 JSON。
type Decision struct {
	Behavior     string          `json:"behavior"` // "allow" 或 "deny"
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	Message      string          `json:"message,omitempty"`
}

// Allow 放行。input 為 nil 時由 MCP 子程序補回原本的工具參數
// （claude 要求 allow 一定要帶 updatedInput，給空物件會變成用空參數執行）。
func Allow(input json.RawMessage) Decision {
	return Decision{Behavior: "allow", UpdatedInput: input}
}

// Deny 拒絕，message 會給 claude 看到。
func Deny(msg string) Decision { return Decision{Behavior: "deny", Message: msg} }

// MCPConfig 產生給 claude --mcp-config 的 JSON 字串，讓 claude 把我們的子程序掛成 MCP server。
func MCPConfig(exe, socket, binding string) string {
	cfg := map[string]any{"mcpServers": map[string]any{
		ServerName: map[string]any{
			"command": exe,
			"args":    []string{"permission-mcp", "--socket", socket, "--binding", binding},
		},
	}}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// ---------- 主程序端：Broker ----------

// Handler 對一則請求做出決策；可以阻塞（例如等使用者回 y/n）。
type Handler func(ctx context.Context, req Request) Decision

// Serve 在 unix socket 上收請求，每條連線一則請求、一則回覆。ctx 取消時關閉。
func Serve(ctx context.Context, socket string, h Handler) error {
	_ = os.Remove(socket) // 上次沒清掉的殘留 socket 檔
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("permission: 監聽 %s: %w", socket, err)
	}
	_ = os.Chmod(socket, 0o600) // 只有自己能連，別人不能冒充 claude 要權限
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				_ = os.Remove(socket)
				return ctx.Err()
			}
			return err
		}
		go handleConn(ctx, conn, h)
	}
}

func handleConn(ctx context.Context, conn net.Conn, h Handler) {
	defer conn.Close()
	var req Request
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil || json.Unmarshal(line, &req) != nil {
		return
	}
	// 子程序斷線（claude 被砍）時取消等待，免得一直掛在頻道上等 y/n
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, conn) // 對方關閉才會返回
		cancel()
	}()
	d := safeHandle(cctx, h, req)
	out, _ := json.Marshal(d)
	_, _ = conn.Write(append(out, '\n'))
}

// safeHandle 呼叫 handler；handler panic 時改回拒絕，不讓一則請求拖垮整個 Muninn。
func safeHandle(ctx context.Context, h Handler, req Request) (d Decision) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "permission: handler panic（%s）: %v\n%s", req.ToolName, r, debug.Stack())
			d = Deny(fmt.Sprintf("Muninn 權限服務內部錯誤，已拒絕：%v", r))
		}
	}()
	return h(ctx, req)
}

// ---------- 子程序端：stdio MCP server ----------

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ServeMCP 在 in/out 上講 MCP（newline-delimited JSON-RPC），把 tools/call 轉給 socket 上的 Broker。
func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, socket, binding string) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue // 通知（沒有 id）不用回
		}
		var err error
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			err = reply(m.ID, map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": ServerName, "version": "0.3"},
			})
		case "tools/list":
			err = reply(m.ID, map[string]any{"tools": []any{map[string]any{
				"name":        ToolName,
				"description": "Muninn 權限確認：把工具呼叫貼到頻道等使用者回覆",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool_name":   map[string]any{"type": "string"},
						"input":       map[string]any{"type": "object"},
						"tool_use_id": map[string]any{"type": "string"},
					},
					"required": []string{"tool_name", "input"},
				},
			}}})
		case "tools/call":
			var p struct {
				Arguments Request `json:"arguments"`
			}
			_ = json.Unmarshal(m.Params, &p)
			req := p.Arguments
			req.Binding = binding
			d := ask(ctx, socket, req)
			text, _ := json.Marshal(d)
			err = reply(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}})
		case "ping":
			err = reply(m.ID, map[string]any{})
		default:
			err = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"error": map[string]any{"code": -32601, "message": "method not found: " + m.Method}})
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}

// ask 把請求送到主程序的 Broker 並等決策；任何錯誤都當拒絕（寧可擋下也不要默默放行）。
func ask(ctx context.Context, socket string, req Request) Decision {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dctx, "unix", socket)
	cancel()
	if err != nil {
		return Deny("Muninn 權限服務連不上，已拒絕：" + err.Error())
	}
	defer conn.Close()
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Deny("Muninn 權限服務寫入失敗，已拒絕")
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	var dec Decision
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return Deny("Muninn 權限服務沒有回應，已拒絕")
	}
	if json.Unmarshal(line, &dec) != nil || (dec.Behavior != "allow" && dec.Behavior != "deny") {
		return Deny("Muninn 權限服務回了無法解析的決策，已拒絕")
	}
	if dec.Behavior == "allow" && len(dec.UpdatedInput) == 0 {
		dec.UpdatedInput = req.Input
		if len(dec.UpdatedInput) == 0 {
			dec.UpdatedInput = json.RawMessage("{}")
		}
	}
	return dec
}

// ---------- 顯示 ----------

// Describe 把一次工具呼叫整理成給人看的短文字（Discord 用）。
func Describe(tool string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	str := func(k string) string { s, _ := m[k].(string); return s }
	var body string
	switch tool {
	case "Bash":
		body = str("command")
		if d := str("description"); d != "" {
			return fmt.Sprintf("`Bash`：%s\n%s", d, codeBlock(body))
		}
	case "Edit", "MultiEdit", "Write", "NotebookEdit", "Read":
		p := str("file_path")
		if p == "" {
			p = str("notebook_path")
		}
		return fmt.Sprintf("`%s`：`%s`", tool, p)
	case "WebFetch":
		return fmt.Sprintf("`WebFetch`：%s", str("url"))
	default:
		body = string(input)
	}
	return fmt.Sprintf("`%s`\n%s", tool, codeBlock(body))
}

func codeBlock(s string) string {
	const max = 1200
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…（已截斷）"
	}
	// 內容裡的 ``` 會提早結束 code block，插入零寬空白打斷
	s = strings.ReplaceAll(s, "```", "`​``")
	return "```\n" + s + "\n```"
}
