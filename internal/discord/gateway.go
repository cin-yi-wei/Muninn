// Package discord 是 Muninn 需要的最小 Discord 介面：Gateway 收訊息、REST 送訊息。
package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"muninn/internal/chat"
)

// Gateway opcodes（只列用到的）
const (
	opDispatch     = 0
	opHeartbeat    = 1
	opIdentify     = 2
	opReconnect    = 7
	opInvalid      = 9
	opHello        = 10
	opHeartbeatACK = 11
)

// GUILD_MESSAGES (1<<9) | DIRECT_MESSAGES (1<<12) | MESSAGE_CONTENT (1<<15)。
// MESSAGE_CONTENT 是特權 intent，要在 bot 設定頁打開。
const intents = (1 << 9) | (1 << 12) | (1 << 15)

const defaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

// readyTimeout：IDENTIFY 後這麼久還沒收到 READY 就當殭屍連線砍掉重連。
const readyTimeout = 20 * time.Second

// Message 是從 MESSAGE_CREATE 抽出來、我們關心的欄位（與其他平台共用）。
type Message = chat.Message

// Conn 是 gateway 迴圈需要的 websocket 介面，抽出來方便用假連線測試。
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close()
}

type wsConn struct{ c *websocket.Conn }

func (w wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.c.Read(ctx)
	return data, err
}
func (w wsConn) Write(ctx context.Context, data []byte) error {
	return w.c.Write(ctx, websocket.MessageText, data)
}
func (w wsConn) Close() { _ = w.c.CloseNow() }

// Gateway 連上 Discord Gateway（整個 bot 一條連線），把 Channels 接受的頻道的人類訊息交給 OnMessage。
type Gateway struct {
	Token     string
	Channels  func(channelID string) bool                // 回 true 才收這個頻道的訊息
	OnMessage func(ctx context.Context, m Message) error // 回傳錯誤會斷線重連
	// OnInteraction 收按鈕點擊（只會送 Channels 接受的頻道）。錯誤只記 log，不斷線
	OnInteraction func(ctx context.Context, it Interaction)
	URL           string                                              // 選填，測試用
	Dial          func(ctx context.Context, url string) (Conn, error) // 選填，測試用
	Logf          func(format string, args ...any)
}

func (g *Gateway) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Run 持續連線，斷線自動重連（指數退避），直到 ctx 取消。
func (g *Gateway) Run(ctx context.Context) error {
	if g.Token == "" {
		return fmt.Errorf("discord gateway: 缺 token")
	}
	url := g.URL
	if url == "" {
		url = defaultGatewayURL
	}
	dial := g.Dial
	if dial == nil {
		dial = func(ctx context.Context, url string) (Conn, error) {
			c, _, err := websocket.Dial(ctx, url, nil)
			if err != nil {
				return nil, err
			}
			c.SetReadLimit(16 << 20) // READY 事件在大型 bot 上可能很大
			return wsConn{c}, nil
		}
	}
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := dial(ctx, url)
		if err != nil {
			g.logf("gateway: 連線失敗: %v（%s 後重試）", err, backoff)
		} else {
			start := time.Now()
			err = g.loop(ctx, conn)
			conn.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			g.logf("gateway: 斷線: %v", err)
			if time.Since(start) > time.Minute {
				backoff = time.Second // 撐過一分鐘算健康連線，退避歸零
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

type envelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

// loop 跑一條連線：HELLO → IDENTIFY → 心跳 + 分派事件。
func (g *Gateway) loop(ctx context.Context, conn Conn) error {
	first, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	var hello envelope
	if err := json.Unmarshal(first, &hello); err != nil {
		return err
	}
	if hello.Op != opHello {
		return fmt.Errorf("預期 HELLO，收到 op %d", hello.Op)
	}
	var hd struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	_ = json.Unmarshal(hello.D, &hd)
	interval := time.Duration(hd.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		interval = 30 * time.Second
	}

	if err := writeJSON(ctx, conn, map[string]any{
		"op": opIdentify,
		"d": map[string]any{
			"token":      g.Token,
			"intents":    intents,
			"properties": map[string]string{"os": "linux", "browser": "muninn", "device": "muninn"},
		},
	}); err != nil {
		return err
	}

	lctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var seq atomic.Int64
	seq.Store(-1)
	var missedAcks atomic.Int32
	var gotReady atomic.Bool

	// 心跳：連續兩次沒收到 ACK 就判定連線死掉
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				if missedAcks.Add(1) > 2 {
					g.logf("gateway: 心跳沒有 ACK，斷線重連")
					conn.Close()
					return
				}
				var d any
				if s := seq.Load(); s >= 0 {
					d = s
				}
				if writeJSON(lctx, conn, map[string]any{"op": opHeartbeat, "d": d}) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	// READY 逾時看門狗
	go func() {
		select {
		case <-lctx.Done():
		case <-time.After(readyTimeout):
			if !gotReady.Load() {
				g.logf("gateway: %s 內沒收到 READY，斷線重連", readyTimeout)
				conn.Close()
			}
		}
	}()

	for {
		data, err := conn.Read(lctx)
		if err != nil {
			return err
		}
		var ev envelope
		if err := json.Unmarshal(data, &ev); err != nil {
			return err
		}
		if ev.S != nil {
			seq.Store(*ev.S)
		}
		switch ev.Op {
		case opHeartbeatACK:
			missedAcks.Store(0)
		case opHeartbeat:
			// 伺服器要求立刻心跳
			var d any
			if s := seq.Load(); s >= 0 {
				d = s
			}
			if err := writeJSON(lctx, conn, map[string]any{"op": opHeartbeat, "d": d}); err != nil {
				return err
			}
		case opReconnect:
			return fmt.Errorf("伺服器要求重連")
		case opInvalid:
			return fmt.Errorf("session 失效")
		case opDispatch:
			switch ev.T {
			case "READY":
				gotReady.Store(true)
				g.logf("gateway: READY")
			case "INTERACTION_CREATE":
				if it, ok := parseInteraction(ev.D, g.Channels); ok && g.OnInteraction != nil {
					// 另開 goroutine：處理可能要打 HTTP，不能卡住讀取迴圈和心跳
					go g.OnInteraction(lctx, it)
				}
			case "MESSAGE_CREATE":
				m, ok := parseMessage(ev.D, g.Channels)
				if !ok {
					continue
				}
				if g.OnMessage != nil {
					if err := g.OnMessage(lctx, m); err != nil {
						return err
					}
				}
			}
		}
	}
}

// parseMessage 抽出 MESSAGE_CREATE 的欄位；非目標頻道、bot / webhook 訊息、空內容都略過。
func parseMessage(raw json.RawMessage, accept func(string) bool) (Message, bool) {
	var d struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		Content   string `json:"content"`
		Timestamp string `json:"timestamp"`
		WebhookID string `json:"webhook_id"`
		Author    struct {
			ID  string `json:"id"`
			Bot bool   `json:"bot"`
		} `json:"author"`
		Attachments []struct {
			Filename    string `json:"filename"`
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
			Size        int64  `json:"size"`
		} `json:"attachments"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return Message{}, false
	}
	// 只有附件沒有文字的訊息（例如只貼一張截圖）也要收
	if accept == nil || !accept(d.ChannelID) || d.Author.Bot || d.WebhookID != "" || (d.Content == "" && len(d.Attachments) == 0) {
		return Message{}, false
	}
	m := Message{ID: d.ID, ChannelID: d.ChannelID, AuthorID: d.Author.ID, Content: d.Content, CreatedAt: d.Timestamp}
	for _, a := range d.Attachments {
		m.Attachments = append(m.Attachments, chat.Attachment{Name: a.Filename, URL: a.URL, ContentType: a.ContentType, Size: a.Size})
	}
	return m, true
}

// Interaction 是一次按鈕點擊（與其他平台共用）。
type Interaction = chat.Interaction

// parseInteraction 只接受訊息元件互動（type 3）；伺服器頻道的使用者在 member.user，私訊在 user。
func parseInteraction(raw json.RawMessage, accept func(string) bool) (Interaction, bool) {
	type user struct {
		ID string `json:"id"`
	}
	var d struct {
		ID        string `json:"id"`
		Type      int    `json:"type"`
		Token     string `json:"token"`
		ChannelID string `json:"channel_id"`
		Member    *struct {
			User user `json:"user"`
		} `json:"member"`
		User *user `json:"user"`
		Data struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Type != 3 || d.Data.CustomID == "" || accept == nil || !accept(d.ChannelID) {
		return Interaction{}, false
	}
	it := Interaction{ID: d.ID, Token: d.Token, ChannelID: d.ChannelID, CustomID: d.Data.CustomID, MessageID: d.Message.ID}
	if d.Member != nil {
		it.UserID = d.Member.User.ID
	} else if d.User != nil {
		it.UserID = d.User.ID
	}
	return it, true
}

func writeJSON(ctx context.Context, conn Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, data)
}
