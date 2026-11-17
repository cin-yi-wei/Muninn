package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"muninn/internal/admin"
	"muninn/internal/chat"
	"muninn/internal/discord"
	"muninn/internal/registry"
	"muninn/internal/session"
	"muninn/internal/telegram"
)

func newTestDaemon(t *testing.T) (*daemon, string, string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	state := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "muninn.json")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := &daemon{
		cfgPath: cfgPath, exe: "/bin/true", socket: filepath.Join(state, "p.sock"), claudeHome: t.TempDir(),
		discord: &discord.REST{Token: "x"}, tg: &telegram.Bot{Token: "y"}, root: ctx,
		running: map[string]*liveBinding{}, routes: map[string]*liveBinding{}, paused: map[string]registry.Binding{}, tgTold: map[string]bool{},
	}
	d.admin = &admin.Admin{ConfigPath: cfgPath}
	t.Cleanup(d.stopAll)
	return d, repo, state
}

func load(t *testing.T, path, body string) registry.Config {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := registry.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestApplyHotReload(t *testing.T) {
	d, repo, state := newTestDaemon(t)
	b := func(name, ch, extra string) string {
		return `{"name":"` + name + `","channel_id":"` + ch + `","repo":"` + repo + `"` + extra + `}`
	}
	head := `{"state_dir":"` + state + `","allowed_users":["u"],"bindings":[`
	d.apply(load(t, d.cfgPath, head+b("a", "1", "")+","+b("b", "2", "")+","+b("c", "3", "")+`]}`))
	if len(d.running) != 3 || d.live("discord", "2") == nil {
		t.Fatalf("應啟動三個：%v", d.running)
	}
	if !session.Running(filepath.Join(state, "bindings", "a")) {
		t.Fatal("應寫執行中標記")
	}
	aBefore, cBefore := d.running["a"], d.running["c"]

	// a 不變、b 移除、c 改設定、d 新增、e 暫停
	d.apply(load(t, d.cfgPath, head+b("a", "1", "")+","+b("c", "3", `,"auto_approve":true`)+","+
		b("d", "4", `,"platform":"telegram"`)+","+b("e", "5", `,"paused":true`)+`]}`))
	if d.running["a"] != aBefore {
		t.Fatal("沒改的 binding 不該重啟")
	}
	if _, ok := d.running["b"]; ok || d.live("discord", "2") != nil {
		t.Fatal("移除的應停掉")
	}
	if session.Running(filepath.Join(state, "bindings", "b")) {
		t.Fatal("停掉後應清掉執行中標記")
	}
	if d.running["c"] == cBefore || !d.running["c"].app.AutoApprove {
		t.Fatal("改設定的應重啟並套用新設定")
	}
	if d.live("telegram", "4") == nil || d.live("discord", "4") != nil {
		t.Fatal("新增的 telegram binding 應依平台分派")
	}
	if _, ok := d.running["e"]; ok || !d.accepts("discord", "5") {
		t.Fatal("暫停的不該啟動，但要能收 !resume")
	}
	if d.accepts("discord", "999") {
		t.Fatal("沒綁的頻道不收")
	}
}

func TestPausedChannelResume(t *testing.T) {
	d, repo, state := newTestDaemon(t)
	body := `{"state_dir":"` + state + `","allowed_users":["u"],"bindings":[{"name":"p","channel_id":"7","repo":"` + repo + `","paused":true}]}`
	d.apply(load(t, d.cfgPath, body))
	cfg, _ := registry.Load(d.cfgPath)
	if bb, _ := cfg.Find("p"); !bb.Paused {
		t.Fatal("前提：暫停中")
	}
	// 白名單外的人不能恢復（不會改設定；送訊息會因為假 token 失敗，忽略錯誤）
	_ = d.onMessage(context.Background(), "discord", msg("7", "stranger", "!resume"))
	cfg, _ = registry.Load(d.cfgPath)
	if bb, _ := cfg.Find("p"); !bb.Paused {
		t.Fatal("白名單外的人不能恢復")
	}
	_ = d.onMessage(context.Background(), "discord", msg("7", "u", "!resume"))
	cfg, _ = registry.Load(d.cfgPath)
	if bb, _ := cfg.Find("p"); bb.Paused {
		t.Fatal("!resume 應取消暫停")
	}
	data, _ := os.ReadFile(d.cfgPath)
	if strings.Contains(string(data), `"paused"`) {
		t.Fatalf("恢復時應拿掉 paused 欄位：%s", data)
	}
}

func msg(ch, author, content string) chatMessage {
	return chatMessage{ID: "m", ChannelID: ch, AuthorID: author, Content: content, CreatedAt: "2026-11-17T00:00:00Z"}
}

type chatMessage = chat.Message
