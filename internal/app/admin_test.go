package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"muninn/internal/admin"
)

type fakeAdmin struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeAdmin) rec(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}
func (f *fakeAdmin) List() (string, error) { f.rec("list"); return "• `a`", nil }
func (f *fakeAdmin) Bind(_ context.Context, o admin.BindOptions) (string, error) {
	f.rec(fmt.Sprintf("bind %s %s %s ch=%s plat=%s create=%v auto=%v", o.Name, o.Repo, o.Branch, o.ChannelID, o.Platform, o.CreateChannel, o.AutoApprove))
	return "ok bind", nil
}
func (f *fakeAdmin) Unbind(_ context.Context, o admin.UnbindOptions) (string, error) {
	f.rec(fmt.Sprintf("unbind %s del=%v keep=%v", o.Name, o.DeleteChannel, o.KeepWorktree))
	return "ok unbind", nil
}
func (f *fakeAdmin) SetPaused(n string, p bool) (string, error) {
	f.rec(fmt.Sprintf("pause %s %v", n, p))
	return "ok pause", nil
}
func (f *fakeAdmin) Set(n, k, v string) (string, error) {
	f.rec("set " + n + " " + k + "=" + v)
	return "ok set", nil
}

func (f *fakeAdmin) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestControlCommands(t *testing.T) {
	dir := t.TempDir()
	snd, fa := &fakeSender{}, &fakeAdmin{}
	a := newApp(t, dir, &fakeAsker{}, snd, map[string]bool{"u1": true})
	a.Admin = fa
	for _, c := range []string{
		"!list",
		"!bind Feat-X ~/project/Repo feat --base=main",
		"!bind tg ~/r --telegram=-100 --auto-approve",
		"!unbind old --delete-channel",
		"!pause other", "!resume other",
		"!set other model=claude-sonnet-5-5",
		"!bind onlyname",
	} {
		_ = a.OnMessage(context.Background(), msgFrom("m", c))
	}
	waitFor(t, func() bool { return len(fa.snapshot()) == 7 && len(snd.snapshot()) == 8 })
	want := []string{
		"list",
		"bind Feat-X ~/project/Repo feat ch= plat= create=true auto=false", // 名稱大小寫原樣交給 admin 驗證
		"bind tg ~/r  ch=-100 plat=telegram create=false auto=true",
		"unbind old del=true keep=false",
		"pause other true", "pause other false",
		"set other model=claude-sonnet-5-5",
	}
	got := fa.snapshot()
	for i, w := range want {
		if !containsStr(got, w) {
			t.Errorf("缺 %q（第 %d 項），實際 %v", w, i, got)
		}
	}
	all := ""
	for _, s := range snd.snapshot() {
		all += s.text + "\n"
	}
	if !strings.Contains(all, "用法：") {
		t.Fatal("參數不夠應回用法")
	}
	if pendingInbox(dir) != 0 {
		t.Fatal("管理指令不該進 inbox")
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestNonControlChannelPassesThrough(t *testing.T) {
	dir := t.TempDir()
	snd := &fakeSender{}
	a := newApp(t, dir, &fakeAsker{}, snd, nil)
	_ = a.OnMessage(context.Background(), msgFrom("m", "!bind x ~/r"))
	if pendingInbox(dir) != 1 {
		t.Fatal("非控制頻道的 !bind 應交給 claude")
	}
	if r := a.HandleTool("binding_list", nil); !r.IsError {
		t.Fatal("非控制頻道不能用管理工具")
	}
}

func TestSelfPauseAndAdminTools(t *testing.T) {
	snd, fa := &fakeSender{}, &fakeAdmin{}
	a := newApp(t, t.TempDir(), &fakeAsker{}, snd, nil)
	a.Name, a.Pauser, a.Admin = "me", fa, fa
	_ = a.OnMessage(context.Background(), msgFrom("m", "!pause"))
	waitFor(t, func() bool { return containsStr(fa.snapshot(), "pause me true") })
	in, _ := json.Marshal(map[string]any{"name": "n", "repo": "/r", "branch": "b"})
	if r := a.HandleTool("binding_add", in); r.IsError || r.Text != "ok bind" {
		t.Fatalf("%+v", r)
	}
	if !containsStr(fa.snapshot(), "bind n /r b ch= plat= create=true auto=false") {
		t.Fatalf("%v", fa.snapshot())
	}
}
