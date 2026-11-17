// Package telegram 是 Muninn 需要的最小 Telegram Bot API 介面：
// long polling 收訊息與按鈕點擊、送訊息、帶按鈕的訊息、改寫訊息。
//
// 用 long polling（getUpdates），不需要對外開 port 或設定 webhook。
// 注意：同一個 bot 同時只能有一個程序在 getUpdates，否則 Telegram 回 409。
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"muninn/internal/chat"
)

// MaxMessageLen 是 Telegram 單則訊息上限（以字元計）。
const MaxMessageLen = 4096

const defaultAPIBase = "https://api.telegram.org"

// Bot 是 Telegram Bot API client。
type Bot struct {
	Token   string
	APIBase string // 選填，測試用
	HTTP    *http.Client
	Logf    func(format string, args ...any)
}

func (b *Bot) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// APIError 是 Telegram 回 ok=false 時的錯誤。
type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram: %d %s", e.Code, e.Description)
}

// call 呼叫一個 Bot API 方法；429 依 retry_after 等待後重試（最多 5 次）。
func (b *Bot) call(ctx context.Context, method string, params any, result any) error {
	base := b.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: 70 * time.Second} // 要比 long polling 的 50 秒長
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+b.Token+"/"+method, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			// 錯誤訊息裡的 URL 含 token，不要原樣往外丟
			return fmt.Errorf("telegram: %s 連線失敗: %s", method, strings.ReplaceAll(err.Error(), b.Token, "<token>"))
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		var env struct {
			OK          bool            `json:"ok"`
			Result      json.RawMessage `json:"result"`
			ErrorCode   int             `json:"error_code"`
			Description string          `json:"description"`
			Parameters  struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("telegram: %s 回應無法解析（HTTP %d）", method, resp.StatusCode)
		}
		if env.OK {
			if result != nil {
				return json.Unmarshal(env.Result, result)
			}
			return nil
		}
		apiErr := &APIError{Code: env.ErrorCode, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
		if env.ErrorCode == http.StatusTooManyRequests && attempt < 5 {
			wait := time.Duration(max(apiErr.RetryAfter, 1)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return apiErr
	}
}

// sendText 送一段文字：先試 HTML（把 Markdown 轉過去），被 Telegram 拒絕（標記不合法）就退回純文字。
func (b *Bot) sendText(ctx context.Context, params map[string]any, text string) (int64, error) {
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	params["text"] = ToHTML(text)
	params["parse_mode"] = "HTML"
	params["link_preview_options"] = map[string]any{"is_disabled": true}
	err := b.call(ctx, "sendMessage", params, &msg)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest && strings.Contains(apiErr.Description, "parse") {
		params["text"] = text
		delete(params, "parse_mode")
		err = b.call(ctx, "sendMessage", params, &msg)
	}
	return msg.MessageID, err
}

// Send 把 text 切段依序送到聊天室；replyTo 非空時第一段掛成回覆。
func (b *Bot) Send(ctx context.Context, chatID, text, replyTo string) error {
	for i, part := range chat.SplitMessage(text, MaxMessageLen-200) { // 留空間給 HTML 標記膨脹
		params := map[string]any{"chat_id": chatID}
		if i == 0 && replyTo != "" {
			if id, err := strconv.ParseInt(replyTo, 10, 64); err == nil {
				params["reply_parameters"] = map[string]any{"message_id": id, "allow_sending_without_reply": true}
			}
		}
		if _, err := b.sendText(ctx, params, part); err != nil {
			return err
		}
	}
	return nil
}

func keyboard(buttons []chat.Button) map[string]any {
	row := []map[string]any{}
	for _, btn := range buttons {
		row = append(row, map[string]any{"text": btn.Label, "callback_data": btn.CustomID})
	}
	rows := [][]map[string]any{}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return map[string]any{"inline_keyboard": rows}
}

func truncate(s string) string {
	if r := []rune(s); len(r) > MaxMessageLen-300 {
		return string(r[:MaxMessageLen-300]) + "…（已截斷）"
	}
	return s
}

// SendPrompt 送一則帶按鈕的訊息（buttons 為空就是一般訊息），回傳訊息 id。
func (b *Bot) SendPrompt(ctx context.Context, chatID, text string, buttons []chat.Button) (string, error) {
	params := map[string]any{"chat_id": chatID}
	if len(buttons) > 0 {
		params["reply_markup"] = keyboard(buttons)
	}
	id, err := b.sendText(ctx, params, truncate(text))
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

// EditMessage 改寫一則訊息的文字並拿掉按鈕。
func (b *Bot) EditMessage(ctx context.Context, chatID, messageID, text string) error {
	id, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: 訊息 id %q 不合法", messageID)
	}
	text = truncate(text)
	params := map[string]any{"chat_id": chatID, "message_id": id, "text": ToHTML(text), "parse_mode": "HTML",
		"reply_markup": keyboard(nil), "link_preview_options": map[string]any{"is_disabled": true}}
	err = b.call(ctx, "editMessageText", params, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest {
		switch {
		case strings.Contains(apiErr.Description, "not modified"):
			return nil // 內容一樣，不算錯
		case strings.Contains(apiErr.Description, "parse"):
			params["text"] = text
			delete(params, "parse_mode")
			err = b.call(ctx, "editMessageText", params, nil)
		}
	}
	return err
}

// AnswerCallback 回應按鈕點擊（Telegram 要求回應，否則按鈕會一直轉圈）；alert=true 跳出對話框。
func (b *Bot) AnswerCallback(ctx context.Context, callbackID, text string, alert bool) error {
	params := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		params["text"] = text
		params["show_alert"] = alert
	}
	return b.call(ctx, "answerCallbackQuery", params, nil)
}

// Me 回傳 bot 自己的 username（用來辨識群組裡 /cmd@bot 的指令）。
func (b *Bot) Me(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	err := b.call(ctx, "getMe", map[string]any{}, &me)
	return me.Username, err
}

// Command 是 Telegram 輸入框「/」選單裡的一個指令。
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// validate 檢查 Bot API 的限制：command 只能小寫英數底線、1–32 字；description 3–256 字。
func (c Command) validate() error {
	if n := len(c.Command); n < 1 || n > 32 {
		return fmt.Errorf("telegram: 指令 %q 長度須為 1–32", c.Command)
	}
	for _, r := range c.Command {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return fmt.Errorf("telegram: 指令 %q 只能用小寫英數與底線", c.Command)
		}
	}
	if n := len([]rune(c.Description)); n < 3 || n > 256 {
		return fmt.Errorf("telegram: 指令 %q 的說明長度須為 3–256 字", c.Command)
	}
	return nil
}

// MuninnCommands 是單頻道可用的 Muninn 指令（對應 app.helpText），啟動時註冊到 Telegram 選單。
// 控制頻道專用、帶參數的管理指令不放進來。
var MuninnCommands = []Command{
	{"status", "查看 claude 程序狀態與排隊中的訊息"},
	{"usage", "查看 claude 訂閱額度用量與重置時間"},
	{"cancel", "中斷正在跑的這一輪（session 保留）"},
	{"restart", "重啟 claude 程序（session 保留）"},
	{"schedules", "列出這個頻道的定時任務"},
	{"tasks", "列出這個頻道委派出去的子任務"},
	{"pause", "暫停這個頻道（之後用 !resume 恢復）"},
	{"help", "顯示 Muninn 指令說明"},
}

// SetCommands 用 setMyCommands 註冊指令選單（預設範圍：所有聊天室）。送出前先檢查 Bot API 限制。
func (b *Bot) SetCommands(ctx context.Context, cmds []Command) error {
	for _, c := range cmds {
		if err := c.validate(); err != nil {
			return err
		}
	}
	return b.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// ---------- 收訊息 ----------

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Date      int64  `json:"date"`
		Text      string `json:"text"`
		Caption   string `json:"caption"`
		Photo     []struct {
			FileID   string `json:"file_id"`
			FileSize int64  `json:"file_size"`
			Width    int    `json:"width"`
		} `json:"photo"`
		Document *struct {
			FileID   string `json:"file_id"`
			FileName string `json:"file_name"`
			MimeType string `json:"mime_type"`
			FileSize int64  `json:"file_size"`
		} `json:"document"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
	} `json:"message"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

// Poller 用 long polling 收訊息，把 Chats 接受的聊天室的訊息與按鈕點擊交出去。
type Poller struct {
	Bot           *Bot
	Chats         func(chatID string) bool
	OnMessage     func(ctx context.Context, m chat.Message) error
	OnInteraction func(ctx context.Context, it chat.Interaction)
	// OnUnknown 選填：還沒綁定的聊天室傳訊息進來時呼叫（設定新聊天室時用來查 chat id 與 user id）
	OnUnknown func(ctx context.Context, m chat.Message)
	// PollTimeout 是每次 getUpdates 最多等多久，預設 50 秒（測試可調短）
	PollTimeout time.Duration
	// OffsetFile 選填：把已處理到的 update 位置存檔。沒存的話重啟後 Telegram 會重送
	// 24 小時內還沒確認的訊息，可能被重複執行。
	OffsetFile string
}

func (p *Poller) loadOffset() int64 {
	if p.OffsetFile == "" {
		return 0
	}
	b, err := os.ReadFile(p.OffsetFile)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func (p *Poller) saveOffset(n int64) {
	if p.OffsetFile == "" {
		return
	}
	tmp := p.OffsetFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)+"\n"), 0o644); err == nil {
		err = os.Rename(tmp, p.OffsetFile)
		if err != nil {
			p.Bot.logf("telegram: 存 offset 失敗: %v", err)
		}
	}
}

// Run 持續收訊息直到 ctx 取消；網路錯誤會退避重試。
func (p *Poller) Run(ctx context.Context) error {
	timeout := p.PollTimeout
	if timeout <= 0 {
		timeout = 50 * time.Second
	}
	offset := p.loadOffset()
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var updates []update
		err := p.Bot.call(ctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         int(timeout / time.Second),
			"allowed_updates": []string{"message", "callback_query"},
		}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Code == http.StatusConflict {
				p.Bot.logf("telegram: 409 衝突，同一個 bot 有別的程序也在收訊息（或設了 webhook）")
			} else {
				p.Bot.logf("telegram: getUpdates 失敗: %v（%s 後重試）", err, backoff)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1 // 下次 getUpdates 帶上 offset 就等於向 Telegram 確認收到
			}
			if err := p.handle(ctx, u); err != nil {
				p.Bot.logf("telegram: 處理 update %d 失敗: %v", u.UpdateID, err)
			}
		}
		if len(updates) > 0 {
			// 訊息已寫進各 binding 的 inbox（檔案佇列）才存 offset：中途崩潰頂多重收一次，由 inbox 去重
			p.saveOffset(offset)
		}
	}
}

func (p *Poller) handle(ctx context.Context, u update) error {
	if m := u.Message; m != nil {
		text := m.Text
		if text == "" {
			text = m.Caption
		}
		chatID := strconv.FormatInt(m.Chat.ID, 10)
		var atts []chat.Attachment
		if len(m.Photo) > 0 {
			// Telegram 同一張照片給好幾種尺寸，取最大的（陣列最後一個）
			ph := m.Photo[len(m.Photo)-1]
			atts = append(atts, chat.Attachment{Name: fmt.Sprintf("photo_%d.jpg", m.MessageID), ContentType: "image/jpeg", Size: ph.FileSize, FileID: ph.FileID})
		}
		if d := m.Document; d != nil {
			name := d.FileName
			if name == "" {
				name = fmt.Sprintf("file_%d", m.MessageID)
			}
			atts = append(atts, chat.Attachment{Name: name, ContentType: d.MimeType, Size: d.FileSize, FileID: d.FileID})
		}
		// 只有圖片或檔案、沒有文字的訊息也要收
		if (text == "" && len(atts) == 0) || m.From == nil || m.From.IsBot {
			return nil
		}
		msg := chat.Message{
			ID:          strconv.FormatInt(m.MessageID, 10),
			ChannelID:   chatID,
			AuthorID:    strconv.FormatInt(m.From.ID, 10),
			Content:     text,
			CreatedAt:   time.Unix(m.Date, 0).UTC().Format(time.RFC3339),
			Attachments: atts,
		}
		if p.Chats == nil || !p.Chats(chatID) {
			if p.OnUnknown != nil {
				p.OnUnknown(ctx, msg)
			}
			return nil
		}
		if p.OnMessage == nil {
			return nil
		}
		return p.OnMessage(ctx, msg)
	}
	if cq := u.CallbackQuery; cq != nil && cq.Message != nil {
		chatID := strconv.FormatInt(cq.Message.Chat.ID, 10)
		if p.Chats == nil || !p.Chats(chatID) || p.OnInteraction == nil {
			// 不是我們的聊天室也要回應，免得按鈕一直轉圈
			_ = p.Bot.AnswerCallback(ctx, cq.ID, "", false)
			return nil
		}
		go p.OnInteraction(ctx, chat.Interaction{
			ID:        cq.ID,
			ChannelID: chatID,
			UserID:    strconv.FormatInt(cq.From.ID, 10),
			CustomID:  cq.Data,
			MessageID: strconv.FormatInt(cq.Message.MessageID, 10),
		})
	}
	return nil
}

// MaxDownloadBytes 是 Bot API 能下載的檔案上限（20MB）。
const MaxDownloadBytes = 20 << 20

// Fetch 下載附件：先 getFile 換出檔案路徑再下載。下載網址含 bot token，只在這裡用、不落地。
func (b *Bot) Fetch(ctx context.Context, a chat.Attachment) (io.ReadCloser, error) {
	if a.FileID == "" {
		return nil, fmt.Errorf("telegram: 附件沒有 file_id")
	}
	if a.Size > MaxDownloadBytes {
		return nil, fmt.Errorf("檔案 %d MB 超過 Telegram bot 下載上限 20 MB", a.Size>>20)
	}
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := b.call(ctx, "getFile", map[string]any{"file_id": a.FileID}, &f); err != nil {
		return nil, err
	}
	base := b.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/file/bot"+b.Token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: 下載附件失敗: %s", strings.ReplaceAll(err.Error(), b.Token, "<token>"))
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("telegram: 下載附件回 %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// MaxUploadBytes 是 Bot API 上傳上限（50MB）。
const MaxUploadBytes = 50 << 20

// SendFile 上傳檔案：圖片用 sendPhoto（聊天室裡直接顯示），其他用 sendDocument。
func (b *Bot) SendFile(ctx context.Context, chatID, name string, data []byte, caption string) error {
	if len(data) > MaxUploadBytes {
		return fmt.Errorf("檔案 %d MB 超過 Telegram 上限 50 MB", len(data)>>20)
	}
	method, field := "sendDocument", "document"
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		if len(data) <= 10<<20 { // sendPhoto 上限 10MB
			method, field = "sendPhoto", "photo"
		}
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chat_id", chatID)
	if caption != "" {
		if r := []rune(caption); len(r) > 1000 { // caption 上限 1024
			caption = string(r[:1000]) + "…"
		}
		_ = mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	base := b.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+b.Token+"/"+method, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("telegram: 上傳失敗: %s", strings.ReplaceAll(err.Error(), b.Token, "<token>"))
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env)
	if !env.OK {
		return fmt.Errorf("telegram: 上傳檔案失敗：%s", env.Description)
	}
	return nil
}
