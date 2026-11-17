// Package app 把 M1 的元件串起來：
//
//	Discord Gateway → inbox 檔案佇列 → runner（長駐 claude -p）→ outbox 檔案佇列 → Discord REST
//
// 兩段佇列都是檔案：程序崩潰後 pending/ 裡沒處理完的 job 會在重啟時回放。
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"muninn/internal/discord"
	"muninn/internal/queue"
	"muninn/internal/runner"
	"muninn/internal/session"
)

// InboxJob 是一則待交給 claude 的頻道訊息。
type InboxJob struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	AuthorID  string `json:"author_id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// OutboxJob 是一則待送回頻道的回覆。
type OutboxJob struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	ReplyTo   string `json:"reply_to,omitempty"`
	Text      string `json:"text"`
}

// Asker 是 runner 的介面，抽出來方便測試。
type Asker interface {
	Ask(ctx context.Context, text string) (runner.Reply, error)
}

// Sender 是送訊息的介面，抽出來方便測試。
type Sender interface {
	Send(ctx context.Context, channelID, text, replyTo string) error
}

// App 是單一 binding（頻道）的管線。
type App struct {
	StateDir     string
	ChannelID    string
	AllowedUsers map[string]bool // 空 = 不限制（需呼叫端明確允許）
	QuoteReply   bool            // true：回覆掛在原訊息底下（Discord 回覆引用）；預設 false 直接發一般訊息
	AutoApprove  bool            // true：權限請求直接放行，不貼頻道
	// PermissionTimeout 是等使用者回 y/n 的上限，預設 30 分鐘
	PermissionTimeout time.Duration
	Asker             Asker
	Sender            Sender
	Prompter          Prompter // 權限確認用（帶按鈕的訊息）
	Logf              func(format string, args ...any)

	inbox, outbox queue.Dir
	wakeWorker    chan struct{}
	wakeSender    chan struct{}
	perm          permState
}

func (a *App) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Init 建立佇列目錄。
func (a *App) Init() error {
	a.inbox = queue.Dir{Root: filepath.Join(a.StateDir, "inbox")}
	a.outbox = queue.Dir{Root: filepath.Join(a.StateDir, "outbox")}
	a.wakeWorker = make(chan struct{}, 1)
	a.wakeSender = make(chan struct{}, 1)
	if err := a.inbox.Init(); err != nil {
		return err
	}
	return a.outbox.Init()
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// jobID 用訊息建立時間開頭，讓檔名排序就是先進先出；後接 Discord message id 去重。
func jobID(createdAt, messageID string) string {
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		t = time.Now()
	}
	return t.UTC().Format("20060102T150405.000000000") + "-" + messageID
}

// OnMessage 是給 Gateway 的 callback：過濾作者後寫進 inbox。
func (a *App) OnMessage(_ context.Context, m discord.Message) error {
	if len(a.AllowedUsers) > 0 && !a.AllowedUsers[m.AuthorID] {
		a.logf("inbox: 略過非白名單作者 %s 的訊息 %s", m.AuthorID, m.ID)
		return nil
	}
	// 有權限確認在等、而且這則是 y/n：拿去回確認，不進 inbox
	if a.handlePermReply(m.Content) {
		return nil
	}
	job := InboxJob{
		ID: jobID(m.CreatedAt, m.ID), ChannelID: m.ChannelID, MessageID: m.ID,
		AuthorID: m.AuthorID, Content: m.Content, CreatedAt: m.CreatedAt,
	}
	err := a.inbox.Put(job.ID, job)
	if errors.Is(err, queue.ErrExists) {
		return nil // 重連後重送的同一則訊息
	}
	if err != nil {
		// 寫檔失敗不該把 gateway 打斷線，記下來就好
		a.logf("inbox: 寫入失敗: %v", err)
		return nil
	}
	a.logf("inbox: 收到 %s", job.ID)
	wake(a.wakeWorker)
	return nil
}

// RunWorker 依序把 inbox 的 job 交給 claude，回覆寫進 outbox。
func (a *App) RunWorker(ctx context.Context) error {
	return a.drainLoop(ctx, a.wakeWorker, a.inbox, func(id string) error {
		var job InboxJob
		if err := a.inbox.Get(id, &job); err != nil {
			return err
		}
		start := time.Now()
		prompt := job.Content
		// 剛接手舊 session：第一則訊息前面附上交接說明
		handoff := session.TakeHandoff(a.StateDir)
		if handoff != "" {
			prompt = handoff + prompt
		}
		rep, err := a.Asker.Ask(ctx, prompt)
		if ctx.Err() != nil {
			return ctx.Err() // 關機中：job 留在 pending，下次啟動回放
		}
		text := rep.Text
		if err == nil && handoff != "" {
			if cerr := session.ClearHandoff(a.StateDir); cerr != nil {
				a.logf("worker: 清除交接標記失敗: %v", cerr)
			}
		}
		if err != nil {
			text = fmt.Sprintf("⚠️ Muninn：claude 執行失敗：%v", err)
		} else if rep.IsError {
			text = fmt.Sprintf("⚠️ Muninn：claude 回報錯誤（%s）\n%s", rep.Subtype, rep.Text)
		}
		a.logf("worker: %s 完成，耗時 %s，apiKeySource=%q rateLimit=%q cost≈$%.4f（程序累計 $%.4f）",
			id, time.Since(start).Round(time.Millisecond), rep.APIKeySource, rep.RateLimit, rep.CostUSD, rep.TotalCostUSD)
		out := OutboxJob{ID: id, ChannelID: job.ChannelID, Text: text}
		if a.QuoteReply {
			out.ReplyTo = job.MessageID
		}
		if err := a.outbox.Put(out.ID, out); err != nil && !errors.Is(err, queue.ErrExists) {
			return err
		}
		wake(a.wakeSender)
		return a.inbox.Done(id)
	})
}

// RunSender 把 outbox 的回覆送回頻道；送失敗的留在 pending 下一輪重試。
func (a *App) RunSender(ctx context.Context) error {
	return a.drainLoop(ctx, a.wakeSender, a.outbox, func(id string) error {
		var job OutboxJob
		if err := a.outbox.Get(id, &job); err != nil {
			return err
		}
		if err := a.Sender.Send(ctx, job.ChannelID, job.Text, job.ReplyTo); err != nil {
			return err
		}
		a.logf("sender: %s 已送出", id)
		return a.outbox.Done(id)
	})
}

// drainLoop 被叫醒或每 2 秒掃一次 pending，逐筆處理；某筆失敗就停下這輪，等下一輪重試。
func (a *App) drainLoop(ctx context.Context, wakeCh chan struct{}, q queue.Dir, handle func(id string) error) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		ids, err := q.Pending()
		if err != nil {
			a.logf("queue %s: 列出失敗: %v", q.Root, err)
		}
		for _, id := range ids {
			if err := handle(id); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				a.logf("queue %s: 處理 %s 失敗: %v", q.Root, id, err)
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wakeCh:
		case <-tick.C:
		}
	}
}
