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
	"strings"
	"time"

	"muninn/internal/audit"
	"muninn/internal/chat"
	"muninn/internal/queue"
	"muninn/internal/runner"
	"muninn/internal/scheduler"
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
	// Attempts 是送進 claude 的次數。送之前就先 +1 寫回檔案，
	// 所以 Muninn 跑到一半崩潰、重啟回放時看得出這則已經試過幾次。
	Attempts int `json:"attempts,omitempty"`
	// Attachments 是附件參照，處理時才下載
	Attachments []chat.Attachment `json:"attachments,omitempty"`
}

// OutboxJob 是一則待送回頻道的回覆。
type OutboxJob struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	ReplyTo   string `json:"reply_to,omitempty"`
	Text      string `json:"text"`
	// Attempts 是送出失敗的次數；超過上限就放棄，不讓一則壞掉的回覆卡住後面所有回覆
	Attempts int `json:"attempts,omitempty"`
}

// maxSendAttempts：同一則回覆最多送幾次（每輪間隔約 2 秒，涵蓋短暫斷線；永久錯誤也不會卡太久）。
const maxSendAttempts = 15

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
	Prompter          Prompter         // 權限確認用（帶按鈕的訊息）
	Ctl               Controller       // 選填：!status / !cancel / !restart 與閒置、記憶體回收用
	SessionID         string           // 顯示用
	MaxAttempts       int              // 同一則訊息最多送進 claude 幾次（防崩潰無限回放），預設 3
	Schedules         *scheduler.Store // 選填：這個頻道的定時任務
	Activity          bool             // true：claude 跑的時候在頻道顯示正在做什麼（活動旁白）
	Workdir           string           // claude 的工作目錄；旁白裡的路徑會顯示成相對路徑
	Repo              string           // 主 repo 路徑（權限判斷：專案記憶目錄掛在這個 slug 上）
	Name              string           // binding 名稱（自己暫停時用）
	Admin             AdminOps         // 選填：控制頻道才有，管理所有 binding
	Pauser            SelfPauser       // 選填：讓頻道用 !pause 暫停自己
	Tasks             TaskOps          // 選填：委派（delegate 與 task_* 工具）
	Audit             *audit.Log       // 選填：稽核 log
	Files             Files            // 選填：附件下載與傳檔
	StallTimeout      time.Duration    // claude 跑一輪時超過這麼久沒有任何事件就判定卡住，0 表示不檢查
	Logf              func(format string, args ...any)

	inbox, outbox queue.Dir
	wakeWorker    chan struct{}
	wakeSender    chan struct{}
	perm          permState
	auth          authState
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
	// 啟動時就擋下漏接的依賴，不要等到第一個權限請求才 nil panic
	if a.Asker == nil || a.Sender == nil || a.Prompter == nil {
		return fmt.Errorf("app: Asker / Sender / Prompter 都必須設定")
	}
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
func (a *App) OnMessage(_ context.Context, m chat.Message) error {
	if len(a.AllowedUsers) > 0 && !a.AllowedUsers[m.AuthorID] {
		a.logf("inbox: 略過非白名單作者 %s 的訊息 %s", m.AuthorID, m.ID)
		return nil
	}
	// ! 開頭是給 Muninn 自己的指令，不交給 claude（claude 停不了驅動自己的程序）
	if a.handleCommand(m.Content) {
		return nil
	}
	// 有權限確認在等、而且這則是 y/n：拿去回確認，不進 inbox
	if a.handlePermReply(m.Content, m.AuthorID) {
		return nil
	}
	job := InboxJob{
		ID: jobID(m.CreatedAt, m.ID), ChannelID: m.ChannelID, MessageID: m.ID,
		AuthorID: m.AuthorID, Content: m.Content, CreatedAt: m.CreatedAt, Attachments: m.Attachments,
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
		maxAttempts := a.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 3
		}
		job.Attempts++
		if job.Attempts > maxAttempts {
			// 前幾次都沒跑完就讓 Muninn 中斷了，很可能是這則訊息本身有問題：放棄，免得無限回放
			a.logf("worker: %s 已嘗試 %d 次都沒完成，放棄", id, maxAttempts)
			return a.finish(id, job, fmt.Sprintf("⚠️ Muninn：這則訊息已經試了 %d 次，每次都在處理途中中斷（Muninn 崩潰或重啟），放棄處理。\n> %s",
				maxAttempts, firstLine(job.Content)))
		}
		// 登入失效期間不送 claude（一定失敗），job 留著等恢復
		if a.authBlocked() {
			return errAuthBlocked
		}
		if err := a.inbox.Update(id, job); err != nil {
			return err
		}
		start := time.Now()
		prompt := job.Content
		if len(job.Attachments) > 0 {
			a.pruneAttachments()
			prompt += a.downloadAttachments(ctx, id, job.Attachments)
		}
		// 剛接手舊 session：第一則訊息前面附上交接說明
		handoff := session.TakeHandoff(a.StateDir)
		if handoff != "" {
			prompt = handoff + prompt
		}
		askCtx := ctx
		var feed *activityFeed
		if a.Activity {
			feed = a.newActivityFeed()
			askCtx = runner.WithActivity(ctx, feed.add)
		}
		rep, err := a.Asker.Ask(askCtx, prompt)
		if feed != nil {
			switch {
			case ctx.Err() != nil:
				feed.finish("⚪ 中止（Muninn 關機）")
			case errors.Is(err, runner.ErrInterrupted):
				feed.finish("⏹ 已中斷")
			case err != nil || rep.IsError:
				feed.finish("⚠️ 出錯")
			default:
				feed.finish("✅ 完成")
			}
		}
		if ctx.Err() != nil {
			// 正常關機不算一次失敗：次數退回去，job 留在 pending，下次啟動回放
			job.Attempts--
			_ = a.inbox.Update(id, job)
			return ctx.Err()
		}
		if isAuthFailure(rep, err) {
			// 登入失效：這則不算處理過，次數退回去，留在佇列等登入恢復後重跑
			job.Attempts--
			_ = a.inbox.Update(id, job)
			a.markAuthDown(rep, err)
			return errAuthBlocked
		}
		a.markAuthUp()
		text := rep.Text
		if err == nil && handoff != "" {
			if cerr := session.ClearHandoff(a.StateDir); cerr != nil {
				a.logf("worker: 清除交接標記失敗: %v", cerr)
			}
		}
		if errors.Is(err, runner.ErrInterrupted) {
			text = "⏹ 已中斷這一輪（session 保留，可以接著講）"
		} else if err != nil {
			text = fmt.Sprintf("⚠️ Muninn：claude 執行失敗：%v", err)
		} else if rep.IsError {
			text = fmt.Sprintf("⚠️ Muninn：claude 回報錯誤（%s）\n%s", rep.Subtype, rep.Text)
		}
		a.logf("worker: %s 完成，耗時 %s，apiKeySource=%q rateLimit=%q cost≈$%.4f（程序累計 $%.4f）",
			id, time.Since(start).Round(time.Millisecond), rep.APIKeySource, rep.RateLimit, rep.CostUSD, rep.TotalCostUSD)
		return a.finish(id, job, text)
	})
}

// finish 把回覆寫進 outbox，並把 inbox job 標成完成。
func (a *App) finish(id string, job InboxJob, text string) error {
	out := OutboxJob{ID: id, ChannelID: job.ChannelID, Text: text}
	if a.QuoteReply && job.AuthorID != SchedulerAuthor && job.AuthorID != TaskAuthor { // 排程訊息沒有真的 Discord 訊息可以回覆
		out.ReplyTo = job.MessageID
	}
	if err := a.outbox.Put(out.ID, out); err != nil && !errors.Is(err, queue.ErrExists) {
		return err
	}
	wake(a.wakeSender)
	return a.inbox.Done(id)
}

func firstLine(s string) string {
	s = strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80]) + "…"
	}
	return s
}

// RunSender 把 outbox 的回覆送回頻道；送失敗的留在 pending 下一輪重試。
func (a *App) RunSender(ctx context.Context) error {
	return a.drainLoop(ctx, a.wakeSender, a.outbox, func(id string) error {
		var job OutboxJob
		if err := a.outbox.Get(id, &job); err != nil {
			return err
		}
		if err := a.Sender.Send(ctx, job.ChannelID, job.Text, job.ReplyTo); err != nil {
			if ctx.Err() != nil {
				return err
			}
			job.Attempts++
			if job.Attempts >= maxSendAttempts {
				a.logf("sender: %s 送了 %d 次都失敗，放棄（內容保留在 outbox/done）：%v", id, job.Attempts, err)
				_ = a.outbox.Update(id, job)
				return a.outbox.Done(id)
			}
			_ = a.outbox.Update(id, job)
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
				if !errors.Is(err, errAuthBlocked) { // 登入失效已另外通知，不要每 2 秒洗 log
					a.logf("queue %s: 處理 %s 失敗: %v", q.Root, id, err)
				}
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
