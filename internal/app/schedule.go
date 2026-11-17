package app

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"muninn/internal/queue"
	"muninn/internal/scheduler"
)

// SchedulerAuthor 是排程訊息的作者 id。
const SchedulerAuthor = "scheduler"

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// InjectScheduled 把一次排程觸發寫進 inbox，當成使用者傳了這則訊息。
//
// 刻意不走 OnMessage：排程訊息不受白名單限制，也**絕不**拿去回權限確認
// （排程內容剛好是 "y" 時，不能替使用者批准任何東西）。
// 用 occurrence 導出的 MessageID 去重，同一個排定時間點只會送一次。
func (a *App) InjectScheduled(f scheduler.Fire) (bool, error) {
	key := unsafeChars.ReplaceAllString(f.MessageID, "_")
	seenDir := filepath.Join(a.StateDir, "scheduled")
	marker := filepath.Join(seenDir, key)

	// 標記檔記著這個 occurrence 對應的 job id，**先寫標記再寫 inbox**：
	//   - 標記寫了、inbox 還沒寫就崩潰 → 下一輪讀到同一個 job id 補寫，不會漏
	//   - inbox 寫過了 → 同一個 job id 的 Put 回 ErrExists，不會重複執行
	// 排隊順序照實際觸發時間，不照排定時間：補跑三天前的 occurrence 不該插到今天的訊息前面
	now := time.Now().UTC()
	jid := jobID(now.Format(time.RFC3339Nano), key)
	if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
		jid = string(b)
	} else {
		if err := os.MkdirAll(seenDir, 0o755); err != nil {
			return false, err
		}
		tmp := marker + ".tmp"
		if err := os.WriteFile(tmp, []byte(jid), 0o644); err != nil {
			return false, err
		}
		if err := os.Rename(tmp, marker); err != nil {
			return false, err
		}
	}
	job := InboxJob{
		ID:        jid,
		ChannelID: a.ChannelID,
		MessageID: f.MessageID,
		AuthorID:  SchedulerAuthor,
		Content:   f.Trigger.Message,
		CreatedAt: now.Format(time.RFC3339),
	}
	err := a.inbox.Put(jid, job)
	if errors.Is(err, queue.ErrExists) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a.logf("scheduler: 注入 %s → %s", f.Trigger.Name, jid)
	wake(a.wakeWorker)
	return true, nil
}
