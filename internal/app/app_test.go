package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"muninn/internal/discord"
	"muninn/internal/queue"
	"muninn/internal/runner"
)

type fakeAsker struct {
	mu   sync.Mutex
	seen []string
}

func (f *fakeAsker) Ask(_ context.Context, text string) (runner.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, text)
	if text == "boom" {
		return runner.Reply{}, errors.New("壞掉")
	}
	return runner.Reply{Text: "回：" + text}, nil
}

type sent struct{ channel, text, replyTo string }

type fakeSender struct {
	mu    sync.Mutex
	fails int // 前幾次送出失敗
	out   []sent
}

func (f *fakeSender) Send(_ context.Context, ch, text, replyTo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("網路斷了")
	}
	f.out = append(f.out, sent{ch, text, replyTo})
	return nil
}

func (f *fakeSender) snapshot() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.out...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待逾時")
}

func newApp(t *testing.T, dir string, asker Asker, s Sender, allowed map[string]bool) *App {
	a := &App{StateDir: dir, ChannelID: "c1", AllowedUsers: allowed, Asker: asker, Sender: s, Logf: func(string, ...any) {}}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPipelineEndToEnd(t *testing.T) {
	dir := t.TempDir()
	asker, snd := &fakeAsker{}, &fakeSender{fails: 1} // 第一次送出失敗，驗證會重試
	a := newApp(t, dir, asker, snd, map[string]bool{"u1": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)

	msg := func(id, author, content, ts string) discord.Message {
		return discord.Message{ID: id, ChannelID: "c1", AuthorID: author, Content: content, CreatedAt: ts}
	}
	_ = a.OnMessage(ctx, msg("m1", "u1", "第一則", "2026-11-17T07:00:00Z"))
	_ = a.OnMessage(ctx, msg("m1", "u1", "第一則", "2026-11-17T07:00:00Z")) // 重送，應去重
	_ = a.OnMessage(ctx, msg("m2", "stranger", "我不在白名單", "2026-11-17T07:00:01Z"))
	_ = a.OnMessage(ctx, msg("m3", "u1", "boom", "2026-11-17T07:00:02Z"))

	waitFor(t, func() bool { return len(snd.snapshot()) == 2 })
	out := snd.snapshot()
	if out[0] != (sent{"c1", "回：第一則", ""}) {
		t.Fatalf("第一則回覆不對：%+v", out[0])
	}
	if out[1].replyTo != "" || !strings.HasPrefix(out[1].text, "⚠️") {
		t.Fatalf("claude 失敗時應回錯誤訊息：%+v", out[1])
	}
	asker.mu.Lock()
	if len(asker.seen) != 2 {
		t.Fatalf("claude 應只被問兩次（去重 + 白名單），實際 %v", asker.seen)
	}
	asker.mu.Unlock()

	waitFor(t, func() bool {
		in, _ := (queue.Dir{Root: dir + "/inbox"}).Pending()
		ob, _ := (queue.Dir{Root: dir + "/outbox"}).Pending()
		return len(in) == 0 && len(ob) == 0
	})
}

func TestReplaysPendingInboxOnRestart(t *testing.T) {
	dir := t.TempDir()
	// 第一個 App 只收訊息、不跑 worker，模擬收到後就崩潰
	a1 := newApp(t, dir, &fakeAsker{}, &fakeSender{}, nil)
	_ = a1.OnMessage(context.Background(), discord.Message{ID: "m1", ChannelID: "c1", AuthorID: "u", Content: "崩潰前收到", CreatedAt: "2026-11-17T07:00:00Z"})

	snd := &fakeSender{}
	a2 := newApp(t, dir, &fakeAsker{}, snd, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a2.RunWorker(ctx)
	go a2.RunSender(ctx)
	waitFor(t, func() bool { return len(snd.snapshot()) == 1 })
	if got := snd.snapshot()[0].text; got != "回：崩潰前收到" {
		t.Fatalf("得到 %q", got)
	}
}

func TestSessionPersisted(t *testing.T) {
	dir := t.TempDir()
	s1, resume, err := LoadOrCreateSession(dir)
	if err != nil || resume || len(s1.ID) != 36 {
		t.Fatalf("首次應新建 uuid：%+v resume=%v err=%v", s1, resume, err)
	}
	s2, resume, err := LoadOrCreateSession(dir)
	if err != nil || !resume || s2.ID != s1.ID {
		t.Fatalf("第二次應沿用並 resume：%+v resume=%v err=%v", s2, resume, err)
	}
}

func TestJobIDSortsByTime(t *testing.T) {
	a := jobID("2026-11-17T07:00:00.5Z", "999")
	b := jobID("2026-11-17T07:00:01Z", "111")
	if !(a < b) {
		t.Fatalf("job id 應依時間排序：%s vs %s", a, b)
	}
}

func TestQuoteReplyOptIn(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, nil)
	a.QuoteReply = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	_ = a.OnMessage(ctx, discord.Message{ID: "m1", ChannelID: "c1", AuthorID: "u", Content: "hi", CreatedAt: "2026-11-17T07:00:00Z"})
	waitFor(t, func() bool { return len(snd.snapshot()) == 1 })
	if got := snd.snapshot()[0].replyTo; got != "m1" {
		t.Fatalf("打開 QuoteReply 時應掛回覆引用，得到 %q", got)
	}
}
