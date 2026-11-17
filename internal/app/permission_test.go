package app

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"muninn/internal/discord"
	"muninn/internal/permission"
	"muninn/internal/queue"
)

func bash(cmd string) permission.Request {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return permission.Request{Binding: "b", ToolName: "Bash", Input: in}
}

var bashReq = bash("touch x")

func msgFrom(id, content string) discord.Message {
	return discord.Message{ID: id, ChannelID: "c1", AuthorID: "u1", Content: content, CreatedAt: "2026-11-17T08:00:00Z"}
}

var btnRE = regexp.MustCompile(`\[btn (\S+) (muninn\|\w+\|\w+)\]`)

// askAsync 在背景發權限請求，等確認訊息貼出來後回傳結果 channel 和按鈕（label → custom_id）。
func askAsync(t *testing.T, a *App, snd *fakeSender, ctx context.Context, req permission.Request) (<-chan permission.Decision, map[string]string) {
	t.Helper()
	before := len(snd.snapshot())
	out := make(chan permission.Decision, 1)
	go func() { out <- a.RequestPermission(ctx, req) }()
	waitFor(t, func() bool { return len(snd.snapshot()) > before })
	text := snd.snapshot()[before].text
	if !strings.Contains(text, "權限確認") {
		t.Fatalf("應貼出確認訊息：%q", text)
	}
	btns := map[string]string{}
	for _, m := range btnRE.FindAllStringSubmatch(text, -1) {
		btns[m[1]] = m[2]
	}
	return out, btns
}

func pendingInbox(dir string) int {
	ids, _ := (queue.Dir{Root: dir + "/inbox"}).Pending()
	return len(ids)
}

func newPermApp(t *testing.T) (*App, *fakeSender, string) {
	dir := t.TempDir()
	snd := &fakeSender{}
	return newApp(t, dir, &fakeAsker{}, snd, map[string]bool{"u1": true}), snd, dir
}

func TestPromptHasThreeButtons(t *testing.T) {
	a, snd, _ := newPermApp(t)
	res, btns := askAsync(t, a, snd, context.Background(), bashReq)
	if len(btns) != 3 || btns["允許一次"] == "" || btns["允許並記住"] == "" || btns["拒絕"] == "" {
		t.Fatalf("應有三顆按鈕：%v", btns)
	}
	r := a.HandleButton(btns["允許一次"], "u1")
	if !r.Handled || !r.Update || !strings.Contains(r.Text, "touch x") || !strings.Contains(r.Text, "已允許一次") {
		t.Fatalf("按鈕結果應保留原請求並標示結果：%+v", r)
	}
	d := <-res
	if d.Behavior != "allow" || string(d.UpdatedInput) != `{"command":"touch x"}` {
		t.Fatalf("應放行且帶回原參數：%+v %s", d, d.UpdatedInput)
	}
	// 同一顆按鈕再按一次：已失效
	if r := a.HandleButton(btns["允許一次"], "u1"); !strings.Contains(r.Text, "失效") {
		t.Fatalf("重複按應顯示失效：%+v", r)
	}
}

func TestButtonRememberThenAutoAllow(t *testing.T) {
	a, snd, _ := newPermApp(t)
	res, btns := askAsync(t, a, snd, context.Background(), bash("git status"))
	r := a.HandleButton(btns["允許並記住"], "u1")
	if !strings.Contains(r.Text, "bash:git status") {
		t.Fatalf("應顯示記住的範圍：%q", r.Text)
	}
	<-res
	n := len(snd.snapshot())
	if d := a.RequestPermission(context.Background(), bash("git status --short")); d.Behavior != "allow" {
		t.Fatalf("記住過的分類應自動放行：%+v", d)
	}
	if len(snd.snapshot()) != n {
		t.Fatal("自動放行不該再貼確認")
	}
	// 子指令不同、或串了別的指令，都還是要問
	for _, cmd := range []string{"git push", "git status && rm -rf /tmp/x"} {
		res, btns := askAsync(t, a, snd, context.Background(), bash(cmd))
		a.HandleButton(btns["拒絕"], "u1")
		if d := <-res; d.Behavior != "deny" {
			t.Fatalf("%q 應被拒：%+v", cmd, d)
		}
	}
}

func TestUnrememberableCommandHasTwoButtons(t *testing.T) {
	a, snd, _ := newPermApp(t)
	res, btns := askAsync(t, a, snd, context.Background(), bash("echo $(whoami)"))
	if len(btns) != 2 || btns["允許並記住"] != "" {
		t.Fatalf("無法拆解的指令不應有記住按鈕：%v", btns)
	}
	a.HandleButton(btns["允許一次"], "u1")
	<-res
}

func TestStrangerButtonRejected(t *testing.T) {
	a, snd, _ := newPermApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	res, btns := askAsync(t, a, snd, ctx, bashReq)
	r := a.HandleButton(btns["允許一次"], "stranger")
	if !r.Handled || r.Update || !strings.Contains(r.Text, "白名單") {
		t.Fatalf("非白名單應只回給按的人看：%+v", r)
	}
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
	waitFor(t, func() bool { e := snd.editsSnapshot(); return len(e) == 1 && strings.Contains(e[0], "已取消") })
}

func TestForeignButtonIgnored(t *testing.T) {
	a, _, _ := newPermApp(t)
	for _, id := range []string{"ccperm|allow|x", "muninn|hack|x", "muninn|allow"} {
		if r := a.HandleButton(id, "u1"); r.Handled {
			t.Errorf("%q 不是我們的按鈕，應忽略：%+v", id, r)
		}
	}
}

func TestTextRepliesAndQueueOrder(t *testing.T) {
	a, snd, dir := newPermApp(t)
	r1, _ := askAsync(t, a, snd, context.Background(), bashReq)
	r2, _ := askAsync(t, a, snd, context.Background(), bash("make build"))

	// 非確認回覆：照常進 inbox，並提醒還在等
	_ = a.OnMessage(context.Background(), msgFrom("m0", "等等，先跟你說別的"))
	if pendingInbox(dir) != 1 {
		t.Fatal("非確認回覆應照常排隊")
	}
	waitFor(t, func() bool { return strings.Contains(snd.snapshot()[len(snd.snapshot())-1].text, "還在等權限 #1") })

	_ = a.OnMessage(context.Background(), msgFrom("m1", "拒絕"))
	if d := <-r1; d.Behavior != "deny" {
		t.Fatalf("第一則應被拒：%+v", d)
	}
	_ = a.OnMessage(context.Background(), msgFrom("m2", "ya"))
	if d := <-r2; d.Behavior != "allow" {
		t.Fatalf("第二則應放行：%+v", d)
	}
	if !a.store().Covers([]string{"bash:make build"}) {
		t.Fatal("ya 應記住")
	}
	waitFor(t, func() bool { return len(snd.editsSnapshot()) == 2 })
	// 沒有待確認時，y 就是一般訊息
	_ = a.OnMessage(context.Background(), msgFrom("m3", "y"))
	if pendingInbox(dir) != 2 {
		t.Fatal("沒有待確認時 y 應進 inbox")
	}
}

func TestPermissionTimeoutDenies(t *testing.T) {
	a, snd, _ := newPermApp(t)
	a.PermissionTimeout = 100 * time.Millisecond
	if d := a.RequestPermission(context.Background(), bashReq); d.Behavior != "deny" {
		t.Fatalf("逾時應拒絕：%+v", d)
	}
	if last := snd.snapshot()[len(snd.snapshot())-1].text; !strings.Contains(last, "逾時") {
		t.Fatalf("應通知逾時：%q", last)
	}
	if e := snd.editsSnapshot(); len(e) != 1 || !strings.Contains(e[0], "逾時") {
		t.Fatalf("確認訊息應改寫成逾時並拿掉按鈕：%v", e)
	}
}

func TestPermissionAutoApprove(t *testing.T) {
	a, snd, _ := newPermApp(t)
	a.AutoApprove = true
	d := a.RequestPermission(context.Background(), bashReq)
	if d.Behavior != "allow" || string(d.UpdatedInput) != `{"command":"touch x"}` || len(snd.snapshot()) != 0 {
		t.Fatalf("auto_approve 應直接放行、不貼頻道：%+v", d)
	}
}

func TestPermissionSendFailureDenies(t *testing.T) {
	snd := &fakeSender{fails: 1}
	a := newApp(t, t.TempDir(), &fakeAsker{}, snd, nil)
	if d := a.RequestPermission(context.Background(), bashReq); d.Behavior != "deny" {
		t.Fatalf("貼不出確認時應拒絕：%+v", d)
	}
}

func TestParseDecision(t *testing.T) {
	for in, want := range map[string]string{"y": actAllow, " YES ": actAllow, "好。": actAllow, "ya": actRemember, "記住": actRemember, "n": actDeny, "No!": actDeny} {
		if got := parseDecision(in); got != want {
			t.Errorf("%q → %q，預期 %q", in, got, want)
		}
	}
	if parseDecision("yes please do it") != "" {
		t.Error("一般句子不該被當成確認回覆")
	}
}
