package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"muninn/internal/permission"
	"muninn/internal/queue"
	"muninn/internal/scheduler"
)

// SchedulerAuthor 是排程訊息的作者 id。
const SchedulerAuthor = "scheduler"

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// scheduledPrompt 是排程送進 claude 的內容：標明是哪個任務觸發的，讓 claude 知道這不是使用者剛打的字。
func scheduledPrompt(f scheduler.Fire) string {
	kind := "定時任務"
	if f.Manual {
		kind = "定時任務（手動觸發測試）"
	}
	return fmt.Sprintf("[Muninn %s「%s」，排定 %s]\n%s", kind, f.Schedule.Name, f.Occurrence.Format("2006-01-02 15:04 MST"), f.Schedule.Message)
}

// InjectScheduled 把一次排程觸發寫進 inbox。
//
// 刻意不走 OnMessage：排程訊息不受白名單限制，也**絕不**拿去回權限確認
// （排程內容剛好是 "y" 時，不能替使用者批准任何東西）。
// 先寫標記（記著 job id）再寫 inbox：崩潰在任一點，同一個排定時間點都只會執行一次。
func (a *App) InjectScheduled(f scheduler.Fire) (bool, error) {
	key := unsafeChars.ReplaceAllString(f.ID, "_")
	seenDir := filepath.Join(a.StateDir, "scheduled")
	marker := filepath.Join(seenDir, key)
	// 排隊順序照實際觸發時間：補跑昨天的任務不該插到今天的訊息前面
	now := time.Now().UTC()
	jid := jobID(now.Format(time.RFC3339Nano), "sched-"+key)
	if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
		jid = string(b)
	} else {
		if err := os.MkdirAll(seenDir, 0o755); err != nil {
			return false, err
		}
		if err := writeFileAtomic(marker, []byte(jid)); err != nil {
			return false, err
		}
	}
	job := InboxJob{ID: jid, ChannelID: a.ChannelID, MessageID: f.ID, AuthorID: SchedulerAuthor,
		Content: scheduledPrompt(f), CreatedAt: now.Format(time.RFC3339)}
	err := a.inbox.Put(jid, job)
	if errors.Is(err, queue.ErrExists) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a.logf("scheduler: 注入 %s → %s", f.Schedule.Name, jid)
	// 頻道上看不到注入的訊息，補一行讓人知道 claude 為什麼開始講話
	go a.notify(fmt.Sprintf("⏰ 定時任務 `%s` 觸發", f.Schedule.Name))
	wake(a.wakeWorker)
	return true, nil
}

func writeFileAtomic(p string, data []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// RunScheduler 每 interval 檢查一次這個 binding 的定時任務。
func (a *App) RunScheduler(ctx context.Context, interval time.Duration) error {
	if a.Schedules == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := a.Schedules.RunOnce(time.Now(), a.InjectScheduled, a.logf); err != nil {
			a.logf("scheduler: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func toolErr(format string, args ...any) permission.ToolResult {
	return permission.ToolResult{Text: fmt.Sprintf(format, args...), IsError: true}
}

// HandleTool 執行 claude 呼叫的頻道工具（schedule_*）。
func (a *App) HandleTool(method string, input json.RawMessage) permission.ToolResult {
	if a.Schedules == nil {
		return toolErr("這個頻道沒有啟用定時任務")
	}
	var in struct {
		Name     string `json:"name"`
		Message  string `json:"message"`
		Cron     string `json:"cron"`
		At       string `json:"at"`
		Timezone string `json:"timezone"`
		CatchUp  bool   `json:"catch_up"`
		Replace  bool   `json:"replace"`
		Paused   bool   `json:"paused"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return toolErr("參數格式錯誤：%v", err)
		}
	}
	now := time.Now()
	tz := a.Schedules.DefaultTZ()
	switch method {
	case "schedule_add":
		s, err := a.Schedules.Add(scheduler.Schedule{Name: in.Name, Message: in.Message, Cron: in.Cron, At: in.At,
			Timezone: in.Timezone, CatchUp: in.CatchUp}, in.Replace, now)
		if err != nil {
			return toolErr("建立失敗：%v", err)
		}
		a.logf("scheduler: claude 建立任務 %s", s.Name)
		go a.notify("🗓 已建立定時任務：" + scheduler.Describe(s, tz, now))
		return permission.ToolResult{Text: "已建立：" + scheduler.Describe(s, tz, now)}
	case "schedule_list":
		return permission.ToolResult{Text: a.scheduleList(now)}
	case "schedule_remove":
		if err := a.Schedules.Remove(in.Name); err != nil {
			return toolErr("%v", err)
		}
		go a.notify(fmt.Sprintf("🗑 已刪除定時任務 `%s`", in.Name))
		return permission.ToolResult{Text: "已刪除 " + in.Name}
	case "schedule_pause":
		if err := a.Schedules.SetPaused(in.Name, in.Paused, now); err != nil {
			return toolErr("%v", err)
		}
		word := "恢復"
		if in.Paused {
			word = "暫停"
		}
		go a.notify(fmt.Sprintf("⏯ 已%s定時任務 `%s`", word, in.Name))
		return permission.ToolResult{Text: "已" + word + " " + in.Name}
	case "schedule_run_now":
		s, err := a.Schedules.Get(in.Name)
		if err != nil {
			return toolErr("%v", err)
		}
		f := scheduler.Fire{Schedule: s, Occurrence: now, Manual: true, ID: fmt.Sprintf("%s@manual-%d", s.Name, now.UnixNano())}
		if _, err := a.InjectScheduled(f); err != nil {
			return toolErr("觸發失敗：%v", err)
		}
		return permission.ToolResult{Text: "已觸發，任務內容會在這一輪結束後送進對話"}
	}
	return toolErr("不認得的工具 %s", method)
}

func (a *App) scheduleList(now time.Time) string {
	list, err := a.Schedules.List()
	if err != nil {
		return "讀取定時任務失敗：" + err.Error()
	}
	if len(list) == 0 {
		return "這個頻道沒有定時任務"
	}
	lines := make([]string, len(list))
	for i, s := range list {
		lines[i] = "• " + scheduler.Describe(s, a.Schedules.DefaultTZ(), now)
	}
	return strings.Join(lines, "\n")
}
