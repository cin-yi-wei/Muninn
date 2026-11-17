// Package chat 是各聊天平台（Discord、Telegram）共用的訊息型別，App 只認這些，不認平台。
package chat

import "unicode/utf8"

// Message 是一則從頻道收到的使用者訊息。
type Message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	AuthorID  string `json:"author_id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	// Attachments 是訊息附帶的檔案（只有參照，真正下載由 Fetcher 在處理時做）
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment 是一個附件的參照。不同平台用不同欄位定位檔案：
// Discord 用 URL（CDN 簽名網址），Telegram 用 FileID（下載網址含 bot token，不能落地存檔）。
type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	URL         string `json:"url,omitempty"`
	FileID      string `json:"file_id,omitempty"`
}

// Button 是訊息底下的一顆按鈕。Style：1 藍紫、2 灰、3 綠、4 紅（Discord 的語意；不支援顏色的平台忽略）。
type Button struct {
	Label    string
	CustomID string
	Style    int
}

// Interaction 是一次按鈕點擊。
type Interaction struct {
	ID        string
	Token     string
	ChannelID string
	UserID    string
	CustomID  string
	MessageID string
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
