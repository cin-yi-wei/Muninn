package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"muninn/internal/chat"
)

func TestToHTML(t *testing.T) {
	cases := map[string]string{
		"a < b & c":                     "a &lt; b &amp; c",
		"**粗體** 與 `x<y`":                "<b>粗體</b> 與 <code>x&lt;y</code>",
		"`**不是粗體**`":                    "<code>**不是粗體**</code>",
		"看 [文件](https://a.b/c?x=1&y=2)": `看 <a href="https://a.b/c?x=1&amp;y=2">文件</a>`,
		"前\n```go\nif a<b {}\n```\n後":   "前\n<pre><code>if a&lt;b {}</code></pre>\n後",
		"```\nno lang\n```":             "<pre><code>no lang</code></pre>",
		"沒收尾 ```code":                   "沒收尾 ```code",
		"**沒收尾":                         "**沒收尾",
	}
	for in, want := range cases {
		if got := ToHTML(in); got != want {
			t.Errorf("%q\n  得到 %q\n  預期 %q", in, got, want)
		}
	}
}

// fakeAPI 模擬 Telegram Bot API，記錄每次呼叫。
type fakeAPI struct {
	mu    sync.Mutex
	calls []struct {
		method string
		params map[string]any
	}
	updates [][]any // 每次 getUpdates 依序回一批
	reject  string  // 對含這個字的 HTML 回 parse 錯誤
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
			w.WriteHeader(404)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/botTOKEN/")
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.calls = append(f.calls, struct {
			method string
			params map[string]any
		}{method, p})
		var result any = true
		switch method {
		case "sendMessage":
			if f.reject != "" && p["parse_mode"] == "HTML" && strings.Contains(p["text"].(string), f.reject) {
				f.mu.Unlock()
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
				return
			}
			result = map[string]any{"message_id": 77}
		case "editMessageText":
			if p["text"] == "same" {
				f.mu.Unlock()
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`))
				return
			}
		case "getUpdates":
			if len(f.updates) == 0 {
				f.mu.Unlock()
				time.Sleep(50 * time.Millisecond)
				_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
				return
			}
			result = f.updates[0]
			f.updates = f.updates[1:]
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
}

func (f *fakeAPI) snapshot() []struct {
	method string
	params map[string]any
} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]struct {
		method string
		params map[string]any
	}(nil), f.calls...)
}

func newBot(t *testing.T, f *fakeAPI) *Bot {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return &Bot{Token: "TOKEN", APIBase: srv.URL, Logf: func(string, ...any) {}}
}

func TestSendSplitsAndFallsBack(t *testing.T) {
	f := &fakeAPI{reject: "BROKEN"}
	b := newBot(t, f)
	ctx := context.Background()
	long := strings.Repeat("字", 5000)
	if err := b.Send(ctx, "-100", long, "12"); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(ctx, "-100", "**BROKEN**", ""); err != nil {
		t.Fatal(err)
	}
	c := f.snapshot()
	if len(c) != 4 {
		t.Fatalf("應呼叫 4 次（2 段 + 1 次 HTML 被拒 + 1 次純文字重送），實際 %d", len(c))
	}
	if c[0].params["reply_parameters"] == nil || c[1].params["reply_parameters"] != nil {
		t.Fatal("只有第一段掛回覆")
	}
	if c[3].params["parse_mode"] != nil || c[3].params["text"] != "**BROKEN**" {
		t.Fatalf("HTML 被拒應退回純文字：%v", c[3].params)
	}
}

func TestPromptEditAnswer(t *testing.T) {
	f := &fakeAPI{}
	b := newBot(t, f)
	ctx := context.Background()
	id, err := b.SendPrompt(ctx, "5", "確認？", []chat.Button{{Label: "允許", CustomID: "muninn|allow|ab"}, {Label: "拒絕", CustomID: "muninn|deny|ab"}})
	if err != nil || id != "77" {
		t.Fatalf("%q %v", id, err)
	}
	if err := b.EditMessage(ctx, "5", "77", "已允許"); err != nil {
		t.Fatal(err)
	}
	if err := b.EditMessage(ctx, "5", "77", "same"); err != nil {
		t.Fatal("內容沒變不該算錯")
	}
	if err := b.AnswerCallback(ctx, "cb1", "不行", true); err != nil {
		t.Fatal(err)
	}
	c := f.snapshot()
	kb := c[0].params["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
	if len(kb) != 2 || kb[0].(map[string]any)["callback_data"] != "muninn|allow|ab" {
		t.Fatalf("按鈕格式不對：%v", kb)
	}
	if rows := c[1].params["reply_markup"].(map[string]any)["inline_keyboard"].([]any); len(rows) != 0 {
		t.Fatal("改寫時應拿掉按鈕")
	}
	if c[3].params["show_alert"] != true || c[3].params["text"] != "不行" {
		t.Fatalf("%v", c[3].params)
	}
}

func TestPollerDispatches(t *testing.T) {
	f := &fakeAPI{updates: [][]any{{
		map[string]any{"update_id": 10, "message": map[string]any{"message_id": 1, "date": 1795000000, "text": "hi",
			"chat": map[string]any{"id": -100}, "from": map[string]any{"id": 42}}},
		map[string]any{"update_id": 11, "message": map[string]any{"message_id": 2, "date": 1795000000, "text": "別的群",
			"chat": map[string]any{"id": -999}, "from": map[string]any{"id": 42}}},
		map[string]any{"update_id": 12, "message": map[string]any{"message_id": 3, "date": 1795000000, "text": "我是 bot",
			"chat": map[string]any{"id": -100}, "from": map[string]any{"id": 7, "is_bot": true}}},
		map[string]any{"update_id": 13, "callback_query": map[string]any{"id": "cb", "data": "muninn|allow|x",
			"from": map[string]any{"id": 42}, "message": map[string]any{"message_id": 77, "chat": map[string]any{"id": -100}}}},
	}}}
	b := newBot(t, f)
	msgs := make(chan chat.Message, 4)
	its := make(chan chat.Interaction, 4)
	p := &Poller{Bot: b, PollTimeout: time.Second,
		Chats:         func(id string) bool { return id == "-100" },
		OnMessage:     func(_ context.Context, m chat.Message) error { msgs <- m; return nil },
		OnInteraction: func(_ context.Context, it chat.Interaction) { its <- it },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	select {
	case m := <-msgs:
		if m.ChannelID != "-100" || m.AuthorID != "42" || m.Content != "hi" || m.ID != "1" || m.CreatedAt == "" {
			t.Fatalf("%+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("沒收到訊息")
	}
	select {
	case it := <-its:
		if it.ID != "cb" || it.UserID != "42" || it.MessageID != "77" || it.CustomID != "muninn|allow|x" {
			t.Fatalf("%+v", it)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("沒收到按鈕點擊")
	}
	select {
	case m := <-msgs:
		t.Fatalf("別的群、bot 的訊息不該收：%+v", m)
	case <-time.After(200 * time.Millisecond):
	}
	// 第二次 getUpdates 應帶 offset 14（確認收到）
	time.Sleep(100 * time.Millisecond)
	for _, c := range f.snapshot() {
		if c.method == "getUpdates" && c.params["offset"].(float64) == 14 {
			return
		}
	}
	t.Fatal("沒有用 offset 確認收到")
}

func TestTokenNotLeakedInErrors(t *testing.T) {
	b := &Bot{Token: "SECRET123", APIBase: "http://127.0.0.1:1"}
	err := b.call(context.Background(), "getMe", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("錯誤訊息不能帶 token：%v", err)
	}
}
