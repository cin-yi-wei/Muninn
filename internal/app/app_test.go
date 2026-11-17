package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"muninn/internal/discord"
	"muninn/internal/queue"
	"muninn/internal/runner"
	"muninn/internal/session"
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
	edits []string
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

// SendPrompt 把帶按鈕的訊息也記進 out（text 後面附上按鈕 custom_id，方便測試抓）。
func (f *fakeSender) SendPrompt(ctx context.Context, ch, text string, buttons []discord.Button) (string, error) {
	for _, b := range buttons {
		text += "\n[btn " + b.Label + " " + b.CustomID + "]"
	}
	f.mu.Lock()
	n := len(f.out)
	f.mu.Unlock()
	if err := f.Send(ctx, ch, text, ""); err != nil {
		return "", err
	}
	return fmt.Sprintf("msg%d", n), nil
}

func (f *fakeSender) EditMessage(_ context.Context, ch, msgID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, msgID+":"+text)
	return nil
}

func (f *fakeSender) editsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.edits...)
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
	if p, ok := s.(Prompter); ok {
		a.Prompter = p
	}
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

func TestHandoffPrefixedOnceAfterAdopt(t *testing.T) {
	dir := t.TempDir()
	if err := session.Adopt(dir, session.Candidate{ID: "11111111-1111-4111-8111-111111111111", Path: "/x.jsonl"}, false); err != nil {
		t.Fatal(err)
	}
	asker, snd := &fakeAsker{}, &fakeSender{}
	a := newApp(t, dir, asker, snd, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	_ = a.OnMessage(ctx, discord.Message{ID: "m1", ChannelID: "c1", AuthorID: "u", Content: "第一則", CreatedAt: "2026-11-17T07:00:00Z"})
	waitFor(t, func() bool { return len(snd.snapshot()) == 1 })
	_ = a.OnMessage(ctx, discord.Message{ID: "m2", ChannelID: "c1", AuthorID: "u", Content: "第二則", CreatedAt: "2026-11-17T07:00:01Z"})
	waitFor(t, func() bool { return len(snd.snapshot()) == 2 })

	asker.mu.Lock()
	defer asker.mu.Unlock()
	if !strings.HasPrefix(asker.seen[0], "[Muninn 交接說明]") || !strings.HasSuffix(asker.seen[0], "第一則") {
		t.Fatalf("接手後第一則應附交接說明：%q", asker.seen[0])
	}
	if asker.seen[1] != "第二則" {
		t.Fatalf("交接說明只該附一次：%q", asker.seen[1])
	}
}

func TestInitRequiresPrompter(t *testing.T) {
	a := &App{StateDir: t.TempDir(), ChannelID: "c1", Asker: &fakeAsker{}, Sender: &fakeSender{}}
	if err := a.Init(); err == nil {
		t.Fatal("沒設 Prompter 應在啟動時就報錯")
	}
}

func TestSenderGivesUpOnPermanentFailure(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{fails: 1000}
	a := newApp(t, dir, &fakeAsker{}, snd, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 直接放一則回覆在 outbox，並把嘗試次數設到快到上限
	_ = a.outbox.Put("x1", OutboxJob{ID: "x1", ChannelID: "c1", Text: "壞掉", Attempts: maxSendAttempts - 2})
	go a.RunSender(ctx)
	waitFor(t, func() bool {
		ids, _ := a.outbox.Pending()
		return len(ids) == 0
	})
}
