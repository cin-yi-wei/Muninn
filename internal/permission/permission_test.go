package permission

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpSession 啟動 ServeMCP，回傳寫入請求、讀取回覆的函式。
func mcpSession(t *testing.T, socket string) (call func(string) map[string]any) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { _ = ServeMCP(context.Background(), inR, outW, socket, "bind-1") }()
	t.Cleanup(func() { inW.Close() })
	rd := bufio.NewReader(outR)
	return func(line string) map[string]any {
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(resp, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
}

func decisionOf(t *testing.T, resp map[string]any) Decision {
	t.Helper()
	text := resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var d Decision
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

const callLine = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"touch x"},"tool_use_id":"tu1"}}}`

func TestMCPRoundTripThroughBroker(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "p.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Request, 1)
	go func() {
		_ = Serve(ctx, socket, func(_ context.Context, r Request) Decision {
			got <- r
			if strings.Contains(string(r.Input), "rm") {
				return Deny("不行")
			}
			return Allow(nil) // 沒帶 updatedInput 時，子程序要補回原參數
		})
	}()
	time.Sleep(50 * time.Millisecond)

	call := mcpSession(t, socket)
	init := call(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if init["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize 應回同樣的 protocolVersion：%v", init)
	}
	tools := call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if tools["result"].(map[string]any)["tools"].([]any)[0].(map[string]any)["name"] != ToolName {
		t.Fatalf("tools/list 不對：%v", tools)
	}

	d := decisionOf(t, call(callLine))
	r := <-got
	if r.Binding != "bind-1" || r.ToolName != "Bash" || r.ToolUseID != "tu1" {
		t.Fatalf("broker 收到的請求不對：%+v", r)
	}
	if d.Behavior != "allow" || string(d.UpdatedInput) != `{"command":"touch x"}` {
		t.Fatalf("allow 應帶回原參數：%+v %s", d, d.UpdatedInput)
	}

	d = decisionOf(t, call(strings.Replace(callLine, "touch x", "rm -rf y", 1)))
	<-got
	if d.Behavior != "deny" || d.Message != "不行" {
		t.Fatalf("應拒絕：%+v", d)
	}
}

func TestMCPDeniesWhenBrokerDown(t *testing.T) {
	call := mcpSession(t, filepath.Join(t.TempDir(), "none.sock"))
	if d := decisionOf(t, call(callLine)); d.Behavior != "deny" {
		t.Fatalf("broker 不在時必須拒絕，不能放行：%+v", d)
	}
}

func TestBrokerCancelsWhenClientGone(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "p.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	go func() {
		_ = Serve(ctx, socket, func(hctx context.Context, _ Request) Decision {
			<-hctx.Done()
			close(cancelled)
			return Deny("x")
		})
	}()
	time.Sleep(50 * time.Millisecond)
	// 模擬 claude 被砍：連上送出請求後直接斷線
	conn, err := netDial(socket)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte(`{"tool_name":"Bash"}` + "\n"))
	time.Sleep(50 * time.Millisecond)
	conn.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("子程序斷線後 handler 應被取消")
	}
}

func TestDescribe(t *testing.T) {
	if s := Describe("Bash", json.RawMessage(`{"command":"ls`+"```"+`","description":"列檔"}`)); !strings.Contains(s, "列檔") || strings.Count(s, "```") != 2 {
		t.Fatalf("Bash 描述不對或 ``` 沒被打斷：%q", s)
	}
	if s := Describe("Write", json.RawMessage(`{"file_path":"/a/b.go","content":"x"}`)); !strings.Contains(s, "/a/b.go") || strings.Contains(s, "content") {
		t.Fatalf("Write 只該顯示路徑：%q", s)
	}
	long := strings.Repeat("字", 3000)
	if s := Describe("Other", json.RawMessage(`{"x":"`+long+`"}`)); !strings.Contains(s, "已截斷") {
		t.Fatal("長內容應截斷")
	}
}

func TestMCPConfigShape(t *testing.T) {
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(MCPConfig("/bin/muninn", "/s.sock", "b1")), &m); err != nil {
		t.Fatal(err)
	}
	s := m.MCPServers[ServerName]
	if s.Command != "/bin/muninn" || strings.Join(s.Args, " ") != "permission-mcp --socket /s.sock --binding b1" {
		t.Fatalf("%+v", s)
	}
}

func TestBrokerSurvivesHandlerPanic(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "p.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	go func() {
		_ = Serve(ctx, socket, func(context.Context, Request) Decision {
			calls++
			if calls == 1 {
				var p *Request
				_ = p.ToolName // 故意 nil panic
			}
			return Allow(nil)
		})
	}()
	time.Sleep(50 * time.Millisecond)
	if d := ask(ctx, socket, Request{ToolName: "Bash"}); d.Behavior != "deny" || !strings.Contains(d.Message, "內部錯誤") {
		t.Fatalf("handler panic 應回拒絕：%+v", d)
	}
	if d := ask(ctx, socket, Request{ToolName: "Bash", Input: json.RawMessage(`{"a":1}`)}); d.Behavior != "allow" {
		t.Fatalf("panic 之後 broker 應該還活著：%+v", d)
	}
}
