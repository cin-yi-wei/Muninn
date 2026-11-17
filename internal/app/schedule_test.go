package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"muninn/internal/scheduler"
)

func fire(msg string) scheduler.Fire {
	return scheduler.Fire{Trigger: scheduler.Trigger{Name: "am", Binding: "b", Message: msg}, MessageID: "sched:am:2026-11-17T01:00:00Z"}
}

func TestInjectScheduledExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	asker, snd := &fakeAsker{}, &fakeSender{}
	a := newApp(t, dir, asker, snd, map[string]bool{"u1": true}) // 有白名單，排程仍要能進
	a.QuoteReply = true
	created, err := a.InjectScheduled(fire("早安，看一下 CI"))
	if err != nil || !created {
		t.Fatalf("第一次應注入：%v %v", created, err)
	}
	if created, _ := a.InjectScheduled(fire("早安，看一下 CI")); created {
		t.Fatal("同一個時間點不該注入兩次")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	waitFor(t, func() bool { return len(snd.snapshot()) == 1 })
	if s := snd.snapshot()[0]; s.text != "回：早安，看一下 CI" || s.replyTo != "" {
		t.Fatalf("排程回覆不對（也不該掛回覆引用）：%+v", s)
	}
	// 已處理完（在 done/）也不該再注入
	if created, _ := a.InjectScheduled(fire("早安，看一下 CI")); created {
		t.Fatal("處理完的時間點不該再注入")
	}
}

func TestInjectScheduledRecoversAfterCrashBetweenMarkerAndInbox(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir, &fakeAsker{}, &fakeSender{}, nil)
	// 模擬：標記寫了、inbox 還沒寫就崩潰
	seen := filepath.Join(dir, "scheduled")
	_ = os.MkdirAll(seen, 0o755)
	_ = os.WriteFile(filepath.Join(seen, "sched_am_2026-11-17T01_00_00Z"), []byte("20261117T090000.000000000-recovered"), 0o644)
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
	if d := <-res; d.Behavior != "deny" || !strings.Contains(d.Message, "取消") {
		t.Fatalf("排程的 y 不能批准權限（應等到逾時／取消）：%+v", d)
	}
	if pendingInbox(dir) != 1 {
		t.Fatal("排程的 y 應當成一般訊息排隊")
	}
}
