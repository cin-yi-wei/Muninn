package app

import (
	"errors"
	"fmt"
	"time"

	"muninn/internal/queue"
)

// ExternalAuthor 是從網頁／API 送進來的訊息作者 id。
const ExternalAuthor = "api"

// InjectExternal 把網頁或外部 API 送來的訊息寫進 inbox（呼叫端已用 token 驗證過身分）。
// 跟排程一樣不走 OnMessage：不受白名單限制，也不會拿去回權限確認。
func (a *App) InjectExternal(source, text string) (string, error) {
	if text == "" {
		return "", errors.New("訊息是空的")
	}
	now := time.Now().UTC()
	key := unsafeChars.ReplaceAllString(source, "_")
	id := jobID(now.Format(time.RFC3339Nano), "ext-"+key)
	job := InboxJob{ID: id, ChannelID: a.ChannelID, MessageID: id, AuthorID: ExternalAuthor,
		Content: fmt.Sprintf("[Muninn 外部訊息，來源：%s]\n%s", source, text), CreatedAt: now.Format(time.RFC3339)}
	if err := a.inbox.Put(id, job); err != nil && !errors.Is(err, queue.ErrExists) {
		return "", err
	}
	// 頻道上看不到這則訊息，補一行讓人知道 claude 為什麼開始動
	go a.notify(fmt.Sprintf("📨 收到外部訊息（%s）：%s", source, firstLine(text)))
	wake(a.wakeWorker)
	return id, nil
}

// Snapshot 是給管理介面看的即時狀態。
type Snapshot struct {
	Alive        bool   `json:"alive"`
	Busy         bool   `json:"busy"`
	PID          int    `json:"pid,omitempty"`
	IdleSeconds  int    `json:"idle_seconds,omitempty"`
	MemoryMB     int64  `json:"memory_mb,omitempty"`
	Queued       int    `json:"queued"`
	WaitingPerms int    `json:"waiting_permissions"`
	SessionID    string `json:"session_id"`
	AuthDown     bool   `json:"auth_down,omitempty"`
}

// Snapshot 回傳目前狀態。
func (a *App) Snapshot() Snapshot {
	s := Snapshot{SessionID: a.SessionID}
	if a.Ctl != nil {
		st := a.Ctl.Status()
		s.Alive, s.Busy, s.PID, s.MemoryMB = st.Alive, st.Busy, st.PID, st.RSSBytes>>20
		s.IdleSeconds = int(st.IdleFor.Seconds())
	}
	ids, _ := a.inbox.Pending()
	s.Queued = len(ids)
	a.perm.mu.Lock()
	s.WaitingPerms = len(a.perm.queue)
	a.perm.mu.Unlock()
	a.auth.mu.Lock()
	s.AuthDown = a.auth.down
	a.auth.mu.Unlock()
	return s
}
