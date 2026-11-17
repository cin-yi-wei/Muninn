package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"muninn/internal/chat"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

// MaxMessageLen 是 Discord 單則訊息的字數上限。
const MaxMessageLen = 2000

const defaultAPIBase = "https://discord.com/api/v10"

// REST 是送訊息用的最小 Discord HTTP client。
type REST struct {
	Token   string
	APIBase string // 選填，測試時指向假伺服器
	HTTP    *http.Client
}

func (r *REST) base() string {
	if r.APIBase != "" {
		return r.APIBase
	}
	return defaultAPIBase
}

func (r *REST) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Send 把 text 切成多則（每則 ≤ 2000 字）依序送到頻道。replyTo 非空時第一則掛成回覆。
func (r *REST) Send(ctx context.Context, channelID, text, replyTo string) error {
	for i, part := range SplitMessage(text, MaxMessageLen) {
		body := map[string]any{
			"content": part,
			// 不讓 claude 的回覆意外 @everyone / @某人
			"allowed_mentions": map[string]any{"parse": []string{}},
		}
		if i == 0 && replyTo != "" {
			body["message_reference"] = map[string]any{"message_id": replyTo, "fail_if_not_exists": false}
		}
		if err := r.post(ctx, "/channels/"+channelID+"/messages", body); err != nil {
			return err
		}
	}
	return nil
}

// post 送一個 JSON POST。
func (r *REST) post(ctx context.Context, path string, body any) error {
	_, err := r.do(ctx, http.MethodPost, path, body)
	return err
}

// do 送一個 JSON 請求並回傳回應內容；遇到 429 依 retry_after 等待後重試（最多 5 次）。
func (r *REST) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, r.base()+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bot "+r.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "DiscordBot (muninn, 0.1)")
		resp, err := r.client().Do(req)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return respBody, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			wait := time.Second
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if json.Unmarshal(respBody, &rl) == nil && rl.RetryAfter > 0 {
				wait = time.Duration(rl.RetryAfter * float64(time.Second))
			} else if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil {
				wait = time.Duration(s * float64(time.Second))
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return nil, fmt.Errorf("discord: %s %s 回 %d: %s", method, path, resp.StatusCode, respBody)
	}
}

// Button 是訊息底下的一顆按鈕（與其他平台共用）。
type Button = chat.Button

func actionRow(buttons []Button) []any {
	if len(buttons) == 0 {
		return []any{} // 空陣列＝移除既有按鈕
	}
	var comps []any
	for _, b := range buttons {
		comps = append(comps, map[string]any{"type": 2, "style": b.Style, "label": b.Label, "custom_id": b.CustomID})
	}
	return []any{map[string]any{"type": 1, "components": comps}}
}

// truncate 把文字截到 Discord 單則上限內（帶按鈕的訊息不能切多則）。
func truncate(s string) string {
	if utf8.RuneCountInString(s) <= MaxMessageLen {
		return s
	}
	return string([]rune(s)[:MaxMessageLen-10]) + "…（已截斷）"
}

// SendPrompt 送一則帶按鈕的訊息，回傳訊息 id（之後用來編輯、拿掉按鈕）。
func (r *REST) SendPrompt(ctx context.Context, channelID, text string, buttons []Button) (string, error) {
	resp, err := r.do(ctx, http.MethodPost, "/channels/"+channelID+"/messages", map[string]any{
		"content":          truncate(text),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components":       actionRow(buttons),
	})
	if err != nil {
		return "", err
	}
	var m struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resp, &m)
	return m.ID, nil
}

// EditMessage 改寫一則訊息的文字，並拿掉所有按鈕。
func (r *REST) EditMessage(ctx context.Context, channelID, messageID, text string) error {
	_, err := r.do(ctx, http.MethodPatch, "/channels/"+channelID+"/messages/"+messageID, map[string]any{
		"content":    truncate(text),
		"components": []any{},
	})
	return err
}

// Interaction 回應類型。
const (
	InteractionReply         = 4 // 回一則新訊息（可設 ephemeral 只給按的人看）
	InteractionUpdateMessage = 7 // 直接改寫按鈕所在的那則訊息
)

// RespondInteraction 回應按鈕互動（Discord 要求 3 秒內回）。UpdateMessage 時會一併拿掉按鈕。
func (r *REST) RespondInteraction(ctx context.Context, id, token string, typ int, text string, ephemeral bool) error {
	data := map[string]any{"content": truncate(text), "allowed_mentions": map[string]any{"parse": []string{}}}
	if typ == InteractionUpdateMessage {
		data["components"] = []any{}
	}
	if ephemeral {
		data["flags"] = 64
	}
	_, err := r.do(ctx, http.MethodPost, "/interactions/"+id+"/"+token+"/callback", map[string]any{"type": typ, "data": data})
	return err
}

// SplitMessage 見 chat.SplitMessage。
func SplitMessage(text string, limit int) []string { return chat.SplitMessage(text, limit) }

// MaxUploadBytes 是 bot 上傳單一檔案的上限（未加成的伺服器為 10MB，保守取 8MB）。
const MaxUploadBytes = 8 << 20

// Fetch 下載附件（Discord 附件是 CDN 網址，不需要 token）。
func (r *REST) Fetch(ctx context.Context, a chat.Attachment) (io.ReadCloser, error) {
	if a.URL == "" {
		return nil, fmt.Errorf("discord: 附件沒有網址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "DiscordBot (muninn, 0.1)")
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("discord: 下載附件回 %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// SendFile 上傳一個檔案到頻道（multipart），caption 當訊息文字。
func (r *REST) SendFile(ctx context.Context, channelID, name string, data []byte, caption string) error {
	if len(data) > MaxUploadBytes {
		return fmt.Errorf("檔案 %d MB 超過 Discord 上限 %d MB", len(data)>>20, MaxUploadBytes>>20)
	}
	payload, _ := json.Marshal(map[string]any{
		"content":          truncate(caption),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"attachments":      []any{map[string]any{"id": 0, "filename": name}},
	})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	pw, _ := mw.CreateFormField("payload_json")
	_, _ = pw.Write(payload)
	fw, err := mw.CreateFormFile("files[0]", name)
	if err != nil {
		return err
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base()+"/channels/"+channelID+"/messages", bytes.NewReader(body.Bytes()))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+r.Token)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("User-Agent", "DiscordBot (muninn, 0.1)")
		resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
		if err != nil {
			return err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			wait := time.Second
			if json.Unmarshal(respBody, &rl) == nil && rl.RetryAfter > 0 {
				wait = time.Duration(rl.RetryAfter * float64(time.Second))
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return fmt.Errorf("discord: 上傳檔案回 %d: %s", resp.StatusCode, respBody)
	}
}

// CreateChannel 在伺服器裡建一個文字頻道（parentID 是分類，可空），回傳頻道 id。
func (r *REST) CreateChannel(ctx context.Context, guildID, name, parentID, topic string) (string, error) {
	body := map[string]any{"name": name, "type": 0}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	if topic != "" {
		body["topic"] = topic
	}
	resp, err := r.do(ctx, http.MethodPost, "/guilds/"+guildID+"/channels", body)
	if err != nil {
		return "", err
	}
	var ch struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &ch); err != nil || ch.ID == "" {
		return "", fmt.Errorf("discord: 建頻道的回應沒有 id")
	}
	return ch.ID, nil
}

// DeleteChannel 刪除頻道。
func (r *REST) DeleteChannel(ctx context.Context, channelID string) error {
	_, err := r.do(ctx, http.MethodDelete, "/channels/"+channelID, nil)
	return err
}
