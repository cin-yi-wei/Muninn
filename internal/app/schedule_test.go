package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"muninn/internal/scheduler"
)

func fire(msg string) scheduler.Fire {
	return scheduler.Fire{Schedule: scheduler.Schedule{Name: "am", Message: msg},
		Occurrence: time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC), ID: "am@20261117T0100Z"}
}

func withSchedules(a *App) *App {
	a.Schedules = scheduler.NewStore(a.StateDir, "Asia/Taipei")
	return a
}

func TestInjectScheduledExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	asker, snd := &fakeAsker{}, &fakeSender{}
	a := newApp(t, dir, asker, snd, map[string]bool{"u1": true}) // 有白名單，排程仍要能進
	a.QuoteReply = true
	created, err := a.InjectScheduled(fire("看一下 CI"))
	if err != nil || !created {
		t.Fatalf("第一次應注入：%v %v", created, err)
	}
	if created, _ := a.InjectScheduled(fire("看一下 CI")); created {
		t.Fatal("同一個時間點不該注入兩次")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	waitFor(t, func() bool {
		for _, s := range snd.snapshot() {
			if strings.HasPrefix(s.text, "回：") {
				return true
			}
		}
		return false
	})
	asker.mu.Lock()
	prompt := asker.seen[0]
	asker.mu.Unlock()
	if !strings.Contains(prompt, "定時任務「am」") || !strings.HasSuffix(prompt, "看一下 CI") {
		t.Fatalf("送進 claude 的內容應標明是哪個任務：%q", prompt)
	}
	for _, s := range snd.snapshot() {
		if strings.HasPrefix(s.text, "回：") && s.replyTo != "" {
			t.Fatal("排程回覆不該掛回覆引用")
		}
	}
	waitFor(t, func() bool {
		return strings.Contains(snd.snapshot()[0].text+snd.snapshot()[len(snd.snapshot())-1].text, "⏰ 定時任務 `am` 觸發")
	})
	if created, _ := a.InjectScheduled(fire("看一下 CI")); created {
		t.Fatal("處理完的時間點不該再注入")
	}
}

func TestInjectScheduledRecoversAfterCrashBetweenMarkerAndInbox(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir, &fakeAsker{}, &fakeSender{}, nil)
	seen := filepath.Join(dir, "scheduled")
	_ = os.MkdirAll(seen, 0o755)
	_ = os.WriteFile(filepath.Join(seen, "am_20261117T0100Z"), []byte("20261117T090000.000000000-recovered"), 0o644)
	created, err := a.InjectScheduled(fire("x"))
	if err != nil || !created || pendingInbox(dir) != 1 {
		t.Fatalf("應依標記補寫 inbox：%v %v", created, err)
	}
	var job InboxJob
	if err := a.inbox.Get("20261117T090000.000000000-recovered", &job); err != nil {
		t.Fatalf("應沿用標記裡的 job id：%v", err)
	}
}

func TestScheduledYesNeverApprovesPermission(t *testing.T) {
	a, snd, dir := newPermApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, _ := askAsync(t, a, snd, ctx, bashReq)
	if _, err := a.InjectScheduled(fire("y")); err != nil {
		t.Fatal(err)
	}
	if d := <-res; d.Behavior != "deny" {
		t.Fatalf("排程的 y 不能批准權限：%+v", d)
	}
	if pendingInbox(dir) != 1 {
		t.Fatal("排程訊息應當成一般訊息排隊")
	}
}

func call(a *App, method string, args map[string]any) (string, bool) {
	b, _ := json.Marshal(args)
	r := a.HandleTool(method, b)
	return r.Text, r.IsError
}

func TestScheduleTools(t *testing.T) {
	snd := &fakeSender{}
	a := withSchedules(newApp(t, t.TempDir(), &fakeAsker{}, snd, nil))

	if txt, isErr := call(a, "schedule_add", map[string]any{"name": "am-ci", "cron": "0 9 * * 1-5", "message": "看 CI"}); isErr || !strings.Contains(txt, "am-ci") {
		t.Fatalf("建立失敗：%s", txt)
	}
	if txt, isErr := call(a, "schedule_add", map[string]any{"name": "am-ci", "cron": "@daily", "message": "x"}); !isErr || !strings.Contains(txt, "replace") {
		t.Fatalf("同名應提示 replace：%s", txt)
	}
	if txt, isErr := call(a, "schedule_add", map[string]any{"name": "bad", "cron": "0 9 * *", "message": "x"}); !isErr || !strings.Contains(txt, "5 個欄位") {
		t.Fatalf("cron 錯誤應回報：%s", txt)
	}
	at := time.Now().Add(time.Hour).Format(time.RFC3339)
	if _, isErr := call(a, "schedule_add", map[string]any{"name": "once", "at": at, "message": "提醒"}); isErr {
		t.Fatal("單次任務建立失敗")
	}
	list, _ := call(a, "schedule_list", nil)
	if !strings.Contains(list, "am-ci") || !strings.Contains(list, "once") || !strings.Contains(list, "單次") {
		t.Fatalf("列表不對：%s", list)
	}
	if _, isErr := call(a, "schedule_pause", map[string]any{"name": "am-ci", "paused": true}); isErr {
		t.Fatal("暫停失敗")
	}
	if list, _ := call(a, "schedule_list", nil); !strings.Contains(list, "已暫停") {
		t.Fatalf("應顯示暫停：%s", list)
	}
	if _, isErr := call(a, "schedule_run_now", map[string]any{"name": "once"}); isErr || pendingInbox(a.StateDir) != 1 {
		t.Fatal("run_now 應立刻注入")
	}
	if _, isErr := call(a, "schedule_remove", map[string]any{"name": "once"}); isErr {
		t.Fatal("刪除失敗")
	}
	if txt, isErr := call(a, "schedule_remove", map[string]any{"name": "once"}); !isErr || !strings.Contains(txt, "沒有") {
		t.Fatalf("刪不存在的應報錯：%s", txt)
	}
	waitFor(t, func() bool {
		all := ""
		for _, s := range snd.snapshot() {
			all += s.text
		}
		return strings.Contains(all, "已建立定時任務") && strings.Contains(all, "已暫停") && strings.Contains(all, "已刪除")
	})
	// !schedules 指令
	_ = a.OnMessage(context.Background(), msgFrom("m1", "!schedules"))
	waitFor(t, func() bool {
		return strings.Contains(lastText(snd), "定時任務**") && strings.Contains(lastText(snd), "am-ci")
	})
}

func TestScheduleListNeedsNoPermission(t *testing.T) {
	a, snd, _ := newPermApp(t)
	n := len(snd.snapshot())
	req := bashReq
	req.ToolName = "mcp__muninn__schedule_list"
	if d := a.RequestPermission(context.Background(), req); d.Behavior != "allow" || len(snd.snapshot()) != n {
		t.Fatalf("schedule_list 唯讀，應直接放行：%+v", d)
	}
}

func TestRunSchedulerFires(t *testing.T) {
	snd := &fakeSender{}
	a := withSchedules(newApp(t, t.TempDir(), &fakeAsker{}, snd, nil))
	// 直接寫一個「上次檢查在一分鐘前、每分鐘觸發」的任務
	if _, err := a.Schedules.Add(scheduler.Schedule{Name: "tick", Cron: "* * * * *", Message: "嗶"}, false, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunScheduler(ctx, time.Hour)
	waitFor(t, func() bool { return pendingInbox(a.StateDir) == 1 })
}
