package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"muninn/internal/discord"
	"muninn/internal/permission"
	"muninn/internal/queue"
)

var bashReq = permission.Request{Binding: "b", ToolName: "Bash", Input: json.RawMessage(`{"command":"touch x"}`)}

func msgFrom(id, content string) discord.Message {
	return discord.Message{ID: id, ChannelID: "c1", AuthorID: "u1", Content: content, CreatedAt: "2026-11-17T08:00:00Z"}
}

// askAsync 在背景發權限請求，等確認訊息貼出來後回傳結果 channel。
func askAsync(t *testing.T, a *App, snd *fakeSender, ctx context.Context) <-chan permission.Decision {
	t.Helper()
	before := len(snd.snapshot())
	out := make(chan permission.Decision, 1)
	go func() { out <- a.RequestPermission(ctx, bashReq) }()
	waitFor(t, func() bool { return len(snd.snapshot()) > before })
	if s := snd.snapshot()[before]; !strings.Contains(s.text, "權限確認") || !strings.Contains(s.text, "touch x") {
		t.Fatalf("應貼出確認訊息：%q", s.text)
	}
	return out
}

func pendingInbox(dir string) int {
	ids, _ := (queue.Dir{Root: dir + "/inbox"}).Pending()
	return len(ids)
}

func TestPermissionYesKeepsOriginalInput(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, map[string]bool{"u1": true})
	res := askAsync(t, a, snd, context.Background())

	_ = a.OnMessage(context.Background(), msgFrom("m1", "Y"))
	d := <-res
	if d.Behavior != "allow" || string(d.UpdatedInput) != `{"command":"touch x"}` {
		t.Fatalf("y 應放行且帶回原參數：%+v %s", d, d.UpdatedInput)
	}
	if pendingInbox(dir) != 0 {
		t.Fatal("y/n 回覆不該進 inbox")
	}
	waitFor(t, func() bool { return strings.Contains(snd.snapshot()[len(snd.snapshot())-1].text, "已允許") })
}

func TestPermissionNoAndQueueOrder(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, map[string]bool{"u1": true})
	r1 := askAsync(t, a, snd, context.Background())
	r2 := askAsync(t, a, snd, context.Background())

	// 非 y/n：照常進 inbox，並提醒還在等
	_ = a.OnMessage(context.Background(), msgFrom("m0", "等等，先跟你說別的"))
	if pendingInbox(dir) != 1 {
		t.Fatal("非 y/n 訊息應照常排隊")
	}
	waitFor(t, func() bool { return strings.Contains(snd.snapshot()[len(snd.snapshot())-1].text, "還在等權限 #1") })

	_ = a.OnMessage(context.Background(), msgFrom("m1", "拒絕"))
	if d := <-r1; d.Behavior != "deny" {
		t.Fatalf("第一則應被拒：%+v", d)
	}
	_ = a.OnMessage(context.Background(), msgFrom("m2", "ok"))
	if d := <-r2; d.Behavior != "allow" {
		t.Fatalf("第二則應放行：%+v", d)
	}
	// 沒有待確認時，y 就是一般訊息
	_ = a.OnMessage(context.Background(), msgFrom("m3", "y"))
	if pendingInbox(dir) != 2 {
		t.Fatal("沒有待確認時 y 應進 inbox")
	}
}

func TestPermissionStrangerCannotApprove(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, map[string]bool{"u1": true})
	ctx, cancel := context.WithCancel(context.Background())
	res := askAsync(t, a, snd, ctx)
	m := msgFrom("m1", "y")
	m.AuthorID = "stranger"
	_ = a.OnMessage(context.Background(), m)
	select {
	case d := <-res:
		t.Fatalf("白名單外的人不能核准：%+v", d)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	if d := <-res; d.Behavior != "deny" {
		t.Fatalf("取消時應拒絕：%+v", d)
	}
}

func TestPermissionTimeoutDenies(t *testing.T) {
	snd := &fakeSender{}
	a := newApp(t, t.TempDir(), &fakeAsker{}, snd, nil)
	a.PermissionTimeout = 100 * time.Millisecond
	if d := a.RequestPermission(context.Background(), bashReq); d.Behavior != "deny" {
		t.Fatalf("逾時應拒絕：%+v", d)
	}
	if last := snd.snapshot()[len(snd.snapshot())-1].text; !strings.Contains(last, "逾時") {
		t.Fatalf("應通知逾時：%q", last)
	}
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	if len(a.perm.queue) != 0 {
		t.Fatal("逾時後應移出佇列")
	}
}

func TestPermissionAutoApprove(t *testing.T) {
	snd := &fakeSender{}
	a := newApp(t, t.TempDir(), &fakeAsker{}, snd, nil)
	a.AutoApprove = true
	d := a.RequestPermission(context.Background(), bashReq)
	if d.Behavior != "allow" || string(d.UpdatedInput) != `{"command":"touch x"}` || len(snd.snapshot()) != 0 {
		t.Fatalf("auto_approve 應直接放行、不貼頻道：%+v", d)
	}
}

func TestPermissionSendFailureDenies(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeAsker{}, &fakeSender{fails: 1}, nil)
	if d := a.RequestPermission(context.Background(), bashReq); d.Behavior != "deny" {
		t.Fatalf("貼不出確認時應拒絕：%+v", d)
	}
}

func TestParseYesNo(t *testing.T) {
	for in, want := range map[string]bool{"y": true, " YES ": true, "好。": true, "允許": true, "n": false, "No!": false, "不要": false} {
		if got, ok := parseYesNo(in); !ok || got != want {
			t.Errorf("%q → %v %v", in, got, ok)
		}
	}
	if _, ok := parseYesNo("yes please do it"); ok {
		t.Error("一般句子不該被當成 y/n")
	}
}
