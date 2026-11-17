package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"muninn/internal/runner"
)

// Controller 是 Muninn 對 claude 程序的控制能力（runner.Runner 實作）。
type Controller interface {
	Interrupt() bool
	Restart(ctx context.Context) error
	CloseIfIdle(ctx context.Context, d time.Duration) bool
	Status() runner.Status
}

const helpText = "**Muninn 指令**（不會交給 claude）\n" +
	"`!status`：claude 程序狀態、記憶體、排隊中的訊息\n" +
	"`!cancel`：中斷 claude 正在跑的這一輪（session 保留）\n" +
	"`!restart`：中斷並重啟 claude 程序（session 保留，下一則訊息會 --resume）\n" +
	"`!schedules`：列出這個頻道的定時任務（要新增、修改直接跟 claude 說）\n" +
	"`!help`：這份說明"

// handleCommand 處理 ! 開頭的 Muninn 指令；回傳 true 表示已處理、不進 inbox。
// 指令立刻執行，不用等 claude 跑完目前這一輪。
func (a *App) handleCommand(content string) bool {
	cmd := strings.ToLower(strings.TrimSpace(content))
	if !strings.HasPrefix(cmd, "!") {
		return false
	}
	switch cmd {
	case "!help":
		go a.notify(helpText)
	case "!schedules":
		if a.Schedules == nil {
			go a.notify("這個頻道沒有啟用定時任務")
		} else {
			go a.notify("🗓 **定時任務**\n" + a.scheduleList(time.Now()))
		}
	case "!status":
		go a.notify(a.statusText())
	case "!cancel":
		if a.Ctl == nil {
			go a.notify("這個 binding 不支援 !cancel")
		} else if a.Ctl.Interrupt() {
			a.logf("control: !cancel 中斷目前這一輪")
			// 回覆由 worker 送出（「已中斷這一輪」），這裡不重複講
		} else {
			go a.notify("claude 目前沒有在跑，沒有東西可以中斷")
		}
	case "!restart":
		if a.Ctl == nil {
			go a.notify("這個 binding 不支援 !restart")
			break
		}
		go func() {
			a.logf("control: !restart")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := a.Ctl.Restart(ctx); err != nil {
				a.notify("🔄 重啟時出錯：" + err.Error())
				return
			}
			a.notify("🔄 claude 程序已關閉，下一則訊息會重新啟動並接回同一個 session")
		}()
	default:
		return false // 不是我們的指令（例如 !important 開頭的一般句子），交給 claude
	}
	return true
}

func (a *App) statusText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Muninn 狀態**\nsession：`%s`\n", a.SessionID)
	if a.Ctl != nil {
		st := a.Ctl.Status()
		switch {
		case st.Busy:
			b.WriteString("claude：🟢 執行中")
		case st.Alive:
			fmt.Fprintf(&b, "claude：🟡 閒置 %s", st.IdleFor.Round(time.Second))
		default:
			b.WriteString("claude：⚪ 沒有在跑（下一則訊息會啟動）")
		}
		if st.Alive {
			fmt.Fprintf(&b, "，pid %d，記憶體 %d MB", st.PID, st.RSSBytes>>20)
		}
		b.WriteString("\n")
	}
	ids, _ := a.inbox.Pending()
	fmt.Fprintf(&b, "排隊中的訊息：%d", len(ids))
	a.perm.mu.Lock()
	np := len(a.perm.queue)
	a.perm.mu.Unlock()
	if np > 0 {
		fmt.Fprintf(&b, "\n等你確認的權限：%d", np)
	}
	return b.String()
}

// RunSupervisor 定期檢查 claude 程序：超過記憶體上限就重啟，閒置太久就關掉（下次來訊息再 --resume）。
// idle、memLimit 為 0 表示不檢查。只在沒有跑任何一輪時動手，不打斷進行中的工作。
func (a *App) RunSupervisor(ctx context.Context, every, idle time.Duration, memLimit int64) error {
	if a.Ctl == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		st := a.Ctl.Status()
		if memLimit > 0 && st.Alive && st.RSSBytes > memLimit {
			if a.Ctl.CloseIfIdle(ctx, 0) {
				a.logf("supervisor: 記憶體 %d MB 超過上限，已重啟", st.RSSBytes>>20)
				a.notify(fmt.Sprintf("🧹 claude 記憶體 %d MB 超過上限 %d MB，已關掉重開（session 保留）", st.RSSBytes>>20, memLimit>>20))
				warned = false
			} else if !warned {
				// 正在跑：不打斷，等這一輪結束後下次檢查再處理
				a.logf("supervisor: 記憶體 %d MB 超過上限，但正在執行，等這一輪結束", st.RSSBytes>>20)
				warned = true
			}
			continue
		}
		if idle > 0 && a.Ctl.CloseIfIdle(ctx, idle) {
			a.logf("supervisor: 閒置超過 %s，已關閉 claude 程序", idle)
		}
	}
}
