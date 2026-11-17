package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// post 送一個 JSON POST；遇到 429 依 retry_after 等待後重試（最多 5 次）。
func (r *REST) post(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base()+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+r.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "DiscordBot (muninn, 0.1)")
		resp, err := r.client().Do(req)
		if err != nil {
			return err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return nil
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
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return fmt.Errorf("discord: POST %s 回 %d: %s", path, resp.StatusCode, respBody)
	}
}

// SplitMessage 把文字切成每段最多 limit 個字元（以 rune 計）。
// 優先在換行處切，其次空白，都沒有才硬切；不會切斷 UTF-8 字元。
func SplitMessage(text string, limit int) []string {
	if text == "" {
		return []string{"（空白回覆）"}
	}
	var parts []string
	for utf8.RuneCountInString(text) > limit {
		runes := []rune(text)
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut == limit {
			for i := limit; i > limit/2; i-- {
				if runes[i-1] == ' ' {
					cut = i
					break
				}
			}
		}
		parts = append(parts, string(runes[:cut]))
		text = string(runes[cut:])
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}
