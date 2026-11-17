package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"muninn/internal/runner"
)

// authAsker 前 failN 次回登入失效，之後正常。
type authAsker struct {
	mu    sync.Mutex
	calls int
	failN int
	exit  bool // true：用「程序提前結束 + stderr」的形式失敗
}

func (f *authAsker) Ask(_ context.Context, text string) (runner.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failN {
		if f.exit {
			return runner.Reply{}, fmt.Errorf("%w: Invalid API key · Please run /login", runner.ErrExited)
		}
		return runner.Reply{IsError: true, Subtype: "success", Text: "Not logged in · Please run /login"}, nil
	}
	return runner.Reply{Text: "回：" + text}, nil
}

func TestIsAuthFailure(t *testing.T) {
	yes := []struct {
		r runner.Reply
		e error
	}{
		{runner.Reply{IsError: true, Text: "Not logged in · Please run /login"}, nil},
		{runner.Reply{}, fmt.Errorf("%w: OAuth token has expired", runner.ErrExited)},
		{runner.Reply{IsError: true, Text: "API Error: 401 Unauthorized"}, nil},
	}
	for _, c := range yes {
		if !isAuthFailure(c.r, c.e) {
			t.Errorf("應判為登入失效：%+v %v", c.r, c.e)
		}
	}
	no := []struct {
		r runner.Reply
		e error
	}{
		{runner.Reply{Text: "教你怎麼 /login 到 GitHub"}, nil}, // 正常回覆提到 /login 不算
		{runner.Reply{IsError: true, Text: "error_max_turns"}, nil},
		{runner.Reply{}, runner.ErrInterrupted},
		{runner.Reply{}, errors.New("網路斷了")},
	}
	for _, c := range no {
		if isAuthFailure(c.r, c.e) {
			t.Errorf("不該判為登入失效：%+v %v", c.r, c.e)
		}
	}
}

func TestAuthDownKeepsMessageAndRecovers(t *testing.T) {
	for _, exit := range []bool{false, true} {
		dir := t.TempDir()
		asker, snd := &authAsker{failN: 1, exit: exit}, &fakeSender{}
		a := newApp(t, dir, asker, snd, nil)
		ctx, cancel := context.WithCancel(context.Background())
		go a.RunWorker(ctx)
		go a.RunSender(ctx)
		_ = a.OnMessage(ctx, msgFrom("m1", "幫我看一下"))
		waitFor(t, func() bool { return strings.Contains(lastText(snd), "登入失效") })
		if pendingInbox(dir) != 1 {
			t.Fatal("登入失效時訊息要留在佇列")
		}
		var job InboxJob
		_ = a.inbox.Get(jobID("2026-11-17T08:00:00Z", "m1"), &job)
		if job.Attempts != 0 {
			t.Fatalf("登入失效不該算一次嘗試：%d", job.Attempts)
		}
		// 期間再來的訊息也不會送進 claude
		_ = a.OnMessage(ctx, msgFrom("m2", "還有這個"))
		time.Sleep(2500 * time.Millisecond)
		asker.mu.Lock()
		if asker.calls != 1 {
			t.Fatalf("登入失效期間不該再呼叫 claude：%d 次", asker.calls)
		}
		asker.mu.Unlock()
		// 模擬重試時間到
		a.auth.mu.Lock()
		a.auth.until = time.Now()
		a.auth.mu.Unlock()
		waitFor(t, func() bool {
			n := 0
			for _, s := range snd.snapshot() {
				if strings.HasPrefix(s.text, "回：") {
					n++
				}
			}
			return n == 2
		})
		waitFor(t, func() bool {
			for _, s := range snd.snapshot() {
				if strings.Contains(s.text, "登入恢復") {
					return true
				}
			}
			return false
		})
		notices := 0
		for _, s := range snd.snapshot() {
			if strings.Contains(s.text, "登入失效") {
				notices++
			}
		}
		if notices != 1 {
			t.Fatalf("登入失效通知只發一次，實際 %d", notices)
		}
		cancel()
	}
}

func TestStallInterrupts(t *testing.T) {
	snd := &fakeSender{}
	a := newApp(t, t.TempDir(), &fakeAsker{}, snd, nil)
	ctl := &fakeCtl{st: runner.Status{Alive: true, Busy: true, Silent: 25 * time.Minute}}
	a.Ctl = ctl
	if a.checkStall(ctl.st, 0) {
		t.Fatal("0 表示不檢查")
	}
	if a.checkStall(runner.Status{Busy: true, Silent: time.Minute}, 20*time.Minute) {
		t.Fatal("還沒超過門檻")
	}
	// 在等權限確認時不算卡住
	a.perm.mu.Lock()
	a.perm.queue = append(a.perm.queue, &pendingPerm{n: 1})
	a.perm.mu.Unlock()
	if a.checkStall(ctl.st, 20*time.Minute) {
		t.Fatal("等權限確認時不該判定卡住")
	}
	a.perm.mu.Lock()
	a.perm.queue = nil
	a.perm.mu.Unlock()
	if !a.checkStall(ctl.st, 20*time.Minute) {
		t.Fatal("超過門檻應中斷")
	}
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "判定卡住") })
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if ctl.interrupts != 1 {
		t.Fatalf("interrupts=%d", ctl.interrupts)
	}
}
