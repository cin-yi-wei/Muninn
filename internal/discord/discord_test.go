package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSplitMessage(t *testing.T) {
	if got := SplitMessage("短訊息", 2000); len(got) != 1 || got[0] != "短訊息" {
		t.Fatalf("短訊息不應切：%q", got)
	}
	long := strings.Repeat("中", 1500) + "\n" + strings.Repeat("文", 1500)
	parts := SplitMessage(long, 2000)
	if len(parts) != 2 || !strings.HasSuffix(parts[0], "\n") {
		t.Fatalf("應在換行處切成兩段，得到 %d 段", len(parts))
	}
	hard := strings.Repeat("字", 4500)
	parts = SplitMessage(hard, 2000)
	if len(parts) != 3 || strings.Join(parts, "") != hard {
		t.Fatalf("硬切後應能拼回原文，得到 %d 段", len(parts))
	}
	for _, p := range parts {
		if n := utf8.RuneCountInString(p); n > 2000 || !utf8.ValidString(p) {
			t.Fatalf("分段超長或切壞 UTF-8：%d", n)
		}
	}
}

func TestRESTSendSplitsAndRetries429(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Header.Get("Authorization") != "Bot tok" || r.URL.Path != "/channels/c1/messages" {
			w.WriteHeader(400)
			return
		}
		if calls == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"retry_after":0.01}`)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	rest := &REST{Token: "tok", APIBase: srv.URL}
	if err := rest.Send(context.Background(), "c1", strings.Repeat("a", 2500), "m9"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("應送出 2 則，實際 %d", len(bodies))
	}
	if _, ok := bodies[0]["message_reference"]; !ok {
		t.Fatal("第一則應掛 reply")
	}
	if _, ok := bodies[1]["message_reference"]; ok {
		t.Fatal("第二則不應掛 reply")
	}
}

// fakeConn 依序吐出預先排好的 frame，記錄寫出的 frame；frame 吐完就阻塞到 Close。
type fakeConn struct {
	mu     sync.Mutex
	frames chan []byte
	writes [][]byte
	closed chan struct{}
	once   sync.Once
}

func newFakeConn(frames ...string) *fakeConn {
	c := &fakeConn{frames: make(chan []byte, len(frames)), closed: make(chan struct{})}
	for _, f := range frames {
		c.frames <- []byte(f)
	}
	return c
}

func (c *fakeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case f := <-c.frames:
		return f, nil
	case <-c.closed:
		return nil, errors.New("closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *fakeConn) Write(_ context.Context, d []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, d)
	return nil
}
func (c *fakeConn) Close() { c.once.Do(func() { close(c.closed) }) }

func TestGatewayIdentifiesAndFiltersMessages(t *testing.T) {
	conn := newFakeConn(
		`{"op":10,"d":{"heartbeat_interval":45000}}`,
		`{"op":0,"t":"READY","s":1,"d":{"session_id":"x"}}`,
		// 別的頻道 → 略過
		`{"op":0,"t":"MESSAGE_CREATE","s":2,"d":{"id":"1","channel_id":"other","content":"hi","author":{"id":"u"}}}`,
		// bot 自己 → 略過
		`{"op":0,"t":"MESSAGE_CREATE","s":3,"d":{"id":"2","channel_id":"c1","content":"hi","author":{"id":"b","bot":true}}}`,
		// 正常訊息 → 收下
		`{"op":0,"t":"MESSAGE_CREATE","s":4,"d":{"id":"3","channel_id":"c1","content":"開工","timestamp":"2026-11-17T07:28:46.729000+00:00","author":{"id":"u1"}}}`,
	)
	got := make(chan Message, 4)
	g := &Gateway{
		Token: "tok", Channels: func(c string) bool { return c == "c1" },
		OnMessage: func(_ context.Context, m Message) error { got <- m; return nil },
		Dial:      func(context.Context, string) (Conn, error) { return conn, nil },
		Logf:      func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()

	select {
	case m := <-got:
		if m.ID != "3" || m.AuthorID != "u1" || m.Content != "開工" {
			t.Fatalf("收到錯的訊息：%+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("沒收到訊息")
	}
	select {
	case m := <-got:
		t.Fatalf("不該再收到：%+v", m)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	<-done

	conn.mu.Lock()
	defer conn.mu.Unlock()
	var id struct {
		Op int `json:"op"`
		D  struct {
			Token   string `json:"token"`
			Intents int    `json:"intents"`
		} `json:"d"`
	}
	if len(conn.writes) == 0 || json.Unmarshal(conn.writes[0], &id) != nil || id.Op != 2 || id.D.Token != "tok" || id.D.Intents&(1<<15) == 0 {
		t.Fatalf("第一個寫出的 frame 應是帶 MESSAGE_CONTENT intent 的 IDENTIFY：%s", conn.writes)
	}
}

func TestParseInteraction(t *testing.T) {
	accept := func(c string) bool { return c == "c1" }
	guild := `{"id":"i1","type":3,"token":"tk","channel_id":"c1","member":{"user":{"id":"u1"}},"data":{"custom_id":"muninn|allow|ab"},"message":{"id":"m1"}}`
	it, ok := parseInteraction(json.RawMessage(guild), accept)
	if !ok || it.UserID != "u1" || it.CustomID != "muninn|allow|ab" || it.Token != "tk" || it.MessageID != "m1" {
		t.Fatalf("%+v %v", it, ok)
	}
	dm := strings.Replace(guild, `"member":{"user":{"id":"u1"}}`, `"user":{"id":"u2"}`, 1)
	if it, ok := parseInteraction(json.RawMessage(dm), accept); !ok || it.UserID != "u2" {
		t.Fatalf("私訊應從 user 取 id：%+v", it)
	}
	if _, ok := parseInteraction(json.RawMessage(strings.Replace(guild, `"c1"`, `"c9"`, 1)), accept); ok {
		t.Fatal("別的頻道應忽略")
	}
	if _, ok := parseInteraction(json.RawMessage(strings.Replace(guild, `"type":3`, `"type":2`, 1)), accept); ok {
		t.Fatal("非按鈕互動（slash command）應忽略")
	}
}

func TestRESTPromptEditAndInteraction(t *testing.T) {
	type call struct {
		method, path string
		body         map[string]any
	}
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, b})
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"msg42"}`)
	}))
	defer srv.Close()
	rest := &REST{Token: "tok", APIBase: srv.URL}
	ctx := context.Background()

	id, err := rest.SendPrompt(ctx, "c1", "確認？", []Button{{Label: "允許", CustomID: "muninn|allow|x", Style: 3}})
	if err != nil || id != "msg42" {
		t.Fatalf("應回傳訊息 id：%q %v", id, err)
	}
	if err := rest.EditMessage(ctx, "c1", "msg42", "已允許"); err != nil {
		t.Fatal(err)
	}
	if err := rest.RespondInteraction(ctx, "i1", "tk", InteractionUpdateMessage, "已允許", false); err != nil {
		t.Fatal(err)
	}
	if err := rest.RespondInteraction(ctx, "i2", "tk2", InteractionReply, "不行", true); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	row := calls[0].body["components"].([]any)[0].(map[string]any)["components"].([]any)[0].(map[string]any)
	if calls[0].method != "POST" || row["custom_id"] != "muninn|allow|x" || row["type"].(float64) != 2 {
		t.Fatalf("按鈕格式不對：%+v", calls[0])
	}
	if calls[1].method != "PATCH" || calls[1].path != "/channels/c1/messages/msg42" || len(calls[1].body["components"].([]any)) != 0 {
		t.Fatalf("編輯應 PATCH 並清掉按鈕：%+v", calls[1])
	}
	d := calls[2].body["data"].(map[string]any)
	if calls[2].path != "/interactions/i1/tk/callback" || calls[2].body["type"].(float64) != 7 || len(d["components"].([]any)) != 0 {
		t.Fatalf("UPDATE_MESSAGE 格式不對：%+v", calls[2])
	}
	if calls[3].body["data"].(map[string]any)["flags"].(float64) != 64 {
		t.Fatalf("ephemeral 應帶 flags 64：%+v", calls[3])
	}
}
