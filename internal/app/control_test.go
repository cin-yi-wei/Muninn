package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"muninn/internal/discord"
	"muninn/internal/runner"
)

type fakeCtl struct {
	mu          sync.Mutex
	st          runner.Status
	interrupts  int
	restarts    int
	closeIdle   atomic.Int32
	closeResult bool
}

func (f *fakeCtl) Interrupt() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts++
	return f.st.Busy
}
func (f *fakeCtl) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return nil
}
func (f *fakeCtl) CloseIfIdle(context.Context, time.Duration) bool {
	f.closeIdle.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeResult {
		f.st.RSSBytes = 100 << 20 // 重開後記憶體回到正常
	}
	return f.closeResult
}
func (f *fakeCtl) Status() runner.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func lastText(snd *fakeSender) string {
	s := snd.snapshot()
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1].text
}

func TestCommandsBypassInbox(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, map[string]bool{"u1": true})
	ctl := &fakeCtl{st: runner.Status{Alive: true, Busy: true, PID: 42, RSSBytes: 512 << 20}}
	a.Ctl, a.SessionID = ctl, "sess-1"

	_ = a.OnMessage(context.Background(), msgFrom("m1", "!status"))
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "執行中") })
	if s := lastText(snd); !strings.Contains(s, "sess-1") || !strings.Contains(s, "512 MB") || !strings.Contains(s, "排隊中的訊息：0") {
		t.Fatalf("狀態內容不對：%q", s)
	}
	_ = a.OnMessage(context.Background(), msgFrom("m2", "!cancel"))
	_ = a.OnMessage(context.Background(), msgFrom("m3", "!restart"))
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "已關閉") })
	_ = a.OnMessage(context.Background(), msgFrom("m4", "!help"))
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "Muninn 指令") })
	ctl.mu.Lock()
	if ctl.interrupts != 1 || ctl.restarts != 1 {
		t.Fatalf("interrupts=%d restarts=%d", ctl.interrupts, ctl.restarts)
	}
	ctl.mu.Unlock()
	if pendingInbox(dir) != 0 {
		t.Fatal("! 指令不該進 inbox")
	}
	// 不認得的 ! 開頭句子照常交給 claude；白名單外的人下指令無效
	_ = a.OnMessage(context.Background(), msgFrom("m5", "!important 幫我看一下"))
	m := msgFrom("m6", "!restart")
	m.AuthorID = "stranger"
	_ = a.OnMessage(context.Background(), m)
	time.Sleep(50 * time.Millisecond)
	if pendingInbox(dir) != 1 {
		t.Fatal("不認得的 ! 句子應進 inbox")
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if ctl.restarts != 1 {
		t.Fatal("白名單外的人不能下指令")
	}
}

func TestCancelWhenIdle(t *testing.T) {
	snd := &fakeSender{}
	a := newApp(t, t.TempDir(), &fakeAsker{}, snd, nil)
	a.Ctl = &fakeCtl{}
	_ = a.OnMessage(context.Background(), msgFrom("m1", "!cancel"))
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "沒有在跑") })
}

// interruptAsker 回 ErrInterrupted，模擬 !cancel 中斷。
type interruptAsker struct{}

func (interruptAsker) Ask(context.Context, string) (runner.Reply, error) {
	return runner.Reply{}, runner.ErrInterrupted
}

func TestInterruptedReply(t *testing.T) {
	snd := &fakeSender{}
	a := newApp(t, t.TempDir(), interruptAsker{}, snd, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	_ = a.OnMessage(ctx, msgFrom("m1", "跑很久的工作"))
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "已中斷") })
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	dir := t.TempDir()
	asker, snd := &fakeAsker{}, &fakeSender{}
	a := newApp(t, dir, asker, snd, nil)
	// 模擬前面已經試過 3 次、每次都在途中崩潰
	_ = a.OnMessage(context.Background(), discord.Message{ID: "m1", ChannelID: "c1", AuthorID: "u", Content: "會讓 Muninn 崩潰的訊息\n第二行", CreatedAt: "2026-11-17T09:00:00Z"})
	p := filepath.Join(dir, "inbox", "pending", jobID("2026-11-17T09:00:00Z", "m1")+".json")
	var job InboxJob
	data, _ := os.ReadFile(p)
	_ = json.Unmarshal(data, &job)
	job.Attempts = 3
	data, _ = json.Marshal(job)
	_ = os.WriteFile(p, data, 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "放棄處理") })
	if s := lastText(snd); !strings.Contains(s, "會讓 Muninn 崩潰的訊息") || strings.Contains(s, "第二行") {
		t.Fatalf("放棄訊息應引用第一行：%q", s)
	}
	asker.mu.Lock()
	defer asker.mu.Unlock()
	if len(asker.seen) != 0 {
		t.Fatal("超過次數就不該再送進 claude")
	}
}

// blockAsker 卡住直到 ctx 取消，模擬關機時正在跑。
type blockAsker struct{ started chan struct{} }

func (b blockAsker) Ask(ctx context.Context, _ string) (runner.Reply, error) {
	close(b.started)
	<-ctx.Done()
	return runner.Reply{}, ctx.Err()
}

func TestShutdownDoesNotCountAttempt(t *testing.T) {
	dir := t.TempDir()
	b := blockAsker{started: make(chan struct{})}
	a := newApp(t, dir, b, &fakeSender{}, nil)
	_ = a.OnMessage(context.Background(), discord.Message{ID: "m1", ChannelID: "c1", AuthorID: "u", Content: "x", CreatedAt: "2026-11-17T09:00:00Z"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.RunWorker(ctx); close(done) }()
	<-b.started
	cancel()
	<-done
	var job InboxJob
	if err := a.inbox.Get(jobID("2026-11-17T09:00:00Z", "m1"), &job); err != nil {
		t.Fatal(err)
	}
	if job.Attempts != 0 {
		t.Fatalf("正常關機不該算一次嘗試：attempts=%d", job.Attempts)
	}
}

func TestSupervisorIdleAndMemory(t *testing.T) {
	snd := &fakeSender{}
	a := newApp(t, t.TempDir(), &fakeAsker{}, snd, nil)
	ctl := &fakeCtl{st: runner.Status{Alive: true, RSSBytes: 3 << 30}, closeResult: true}
	a.Ctl = ctl
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunSupervisor(ctx, 10*time.Millisecond, time.Minute, 2<<30)
	waitFor(t, func() bool { return strings.Contains(lastText(snd), "超過上限") })

	// 重開後記憶體正常：只做閒置回收，不再通知
	n := ctl.closeIdle.Load()
	waitFor(t, func() bool { return ctl.closeIdle.Load() > n+2 })
	if c := len(snd.snapshot()); c != 1 {
		t.Fatalf("閒置回收不該發通知，實際 %d 則", c)
	}
}

func TestSlashCommands(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, nil)
	a.Ctl = &fakeCtl{}
	for _, c := range []string{"/status", "/status@my_muninn_bot", "/start"} {
		_ = a.OnMessage(context.Background(), msgFrom("m", c))
	}
	_ = a.OnMessage(context.Background(), msgFrom("m2", "/tmp 底下有什麼"))
	waitFor(t, func() bool { return len(snd.snapshot()) == 3 })
	if pendingInbox(dir) != 1 {
		t.Fatal("一般句子（含空白）不該被當成指令")
	}
}

func TestFullWidthBang(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, nil)
	a.Ctl = &fakeCtl{}
	for _, c := range []string{"！help", " ！status "} {
		_ = a.OnMessage(context.Background(), msgFrom("m", c))
	}
	_ = a.OnMessage(context.Background(), msgFrom("m2", "！這句不是指令嗎"))
	waitFor(t, func() bool { return len(snd.snapshot()) == 2 })
	if !strings.Contains(snd.snapshot()[0].text+snd.snapshot()[1].text, "Muninn 指令") {
		t.Fatalf("全形！help 應回指令說明：%+v", snd.snapshot())
	}
	if pendingInbox(dir) != 1 {
		t.Fatal("全形驚嘆號的指令不該進 inbox；不認得的句子照常交給 claude")
	}
}

type fakeUsage struct{ block chan struct{} }

func (f fakeUsage) Report(context.Context) string {
	if f.block != nil {
		<-f.block // 模擬正在主動更新
	}
	return "📊 七天：已用 32%"
}

func TestUsageCommandDoesNotBlock(t *testing.T) {
	// 主動更新可能要等上一分鐘，不能卡住訊息處理
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, nil)
	block := make(chan struct{})
	a.Usage = fakeUsage{block: block}
	done := make(chan struct{})
	go func() {
		_ = a.OnMessage(context.Background(), msgFrom("m", "!usage"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("!usage 在更新期間卡住了 OnMessage")
	}
	close(block)
	waitFor(t, func() bool { return len(snd.snapshot()) == 1 })
}

func TestUsageCommand(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, nil)
	a.Ctl = &fakeCtl{}
	_ = a.OnMessage(context.Background(), msgFrom("m", "!usage"))
	waitFor(t, func() bool { return len(snd.snapshot()) == 1 })
	if !strings.Contains(snd.snapshot()[0].text, "沒有接上") {
		t.Fatalf("沒接 Usage 時要說明：%q", snd.snapshot()[0].text)
	}
	a.Usage = fakeUsage{}
	_ = a.OnMessage(context.Background(), msgFrom("m2", "！usage"))
	waitFor(t, func() bool { return len(snd.snapshot()) == 2 })
	if snd.snapshot()[1].text != "📊 七天：已用 32%" || pendingInbox(dir) != 0 {
		t.Fatalf("!usage 應直接回額度、不進 inbox：%+v", snd.snapshot())
	}
}
