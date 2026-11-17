package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"muninn/internal/admin"
	"muninn/internal/app"
	"muninn/internal/audit"
	"muninn/internal/chat"
	"muninn/internal/registry"
	"muninn/internal/runner"
	"muninn/internal/tasks"
)

type nopAsker struct{}

func (nopAsker) Ask(context.Context, string) (runner.Reply, error) {
	return runner.Reply{Text: "ok"}, nil
}
func (nopAsker) Close(context.Context) error { return nil }

type nopOut struct{}

func (nopOut) Send(context.Context, string, string, string) error { return nil }
func (nopOut) SendPrompt(context.Context, string, string, []chat.Button) (string, error) {
	return "1", nil
}
func (nopOut) EditMessage(context.Context, string, string, string) error { return nil }

type backend struct {
	cfgPath string
	apps    map[string]*app.App
	adm     *admin.Admin
	tm      *tasks.Manager
	log     *audit.Log
}

func (b *backend) Config() registry.Config { c, _ := registry.Load(b.cfgPath); return c }
func (b *backend) App(name string) *app.App {
	if a := b.apps[name]; a != nil && !b.Config().Bindings[0].Paused {
		return a
	}
	return nil
}
func (b *backend) Admin() *admin.Admin   { return b.adm }
func (b *backend) Tasks() *tasks.Manager { return b.tm }
func (b *backend) Audit() *audit.Log     { return b.log }

func setup(t *testing.T) (*httptest.Server, *backend) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	state := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "muninn.json")
	_ = os.WriteFile(cfgPath, []byte(`{"state_dir":"`+state+`","allowed_users":["u"],"bindings":[{"name":"a","channel_id":"1","repo":"`+repo+`"}]}`), 0o600)
	cfg, _ := registry.Load(cfgPath)
	lg := &audit.Log{Path: filepath.Join(state, "audit.jsonl")}
	a := &app.App{StateDir: cfg.StateDirFor("a"), ChannelID: "1", Name: "a", Repo: repo, Workdir: repo,
		Asker: nopAsker{}, Sender: nopOut{}, Prompter: nopOut{}, Logf: func(string, ...any) {}}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	tm := &tasks.Manager{StateDir: filepath.Join(state, "tasks"), NewAsker: func(tasks.Task, []string) tasks.Asker { return nopAsker{} },
		Deliver: func(_, id, text string) error { return a.DeliverTaskResult(id, text) }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_ = tm.Start(ctx)
	be := &backend{cfgPath: cfgPath, apps: map[string]*app.App{"a": a}, adm: &admin.Admin{ConfigPath: cfgPath, Audit: lg}, tm: tm, log: lg}
	srv := httptest.NewServer((&Server{Token: "T", Backend: be}).Handler())
	t.Cleanup(srv.Close)
	return srv, be
}

func call(t *testing.T, srv *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAuthAndPage(t *testing.T) {
	srv, _ := setup(t)
	if code, _ := call(t, srv, "GET", "/api/bindings", "", ""); code != 401 {
		t.Fatalf("沒 token 應 401：%d", code)
	}
	if code, _ := call(t, srv, "GET", "/api/bindings", "wrong", ""); code != 401 {
		t.Fatal("錯 token 應 401")
	}
	if code, body := call(t, srv, "GET", "/", "", ""); code != 200 || !strings.Contains(body, "Muninn 管理後台") {
		t.Fatalf("網頁不需要 token 就能載入（登入在頁面上做）：%d", code)
	}
	if code, body := call(t, srv, "GET", "/api/bindings", "T", ""); code != 200 || !strings.Contains(body, `"name":"a"`) || !strings.Contains(body, `"running":true`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestMessagePauseSchedulesAudit(t *testing.T) {
	srv, be := setup(t)
	if code, body := call(t, srv, "POST", "/api/bindings/a/message", "T", `{"text":"CI 掛了，幫我看","source":"ci"}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if be.apps["a"].Snapshot().Queued != 1 {
		t.Fatal("訊息應寫進 inbox")
	}
	if code, _ := call(t, srv, "POST", "/api/bindings/nope/message", "T", `{"text":"x"}`); code != 404 {
		t.Fatal("不存在的頻道應 404")
	}
	if code, body := call(t, srv, "POST", "/api/bindings/a/schedules", "T", `{"name":"am","cron":"0 9 * * 1-5","message":"早"}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if _, body := call(t, srv, "GET", "/api/bindings/a/schedules", "T", ""); !strings.Contains(body, `"name":"am"`) || !strings.Contains(body, `"next"`) {
		t.Fatal(body)
	}
	if code, _ := call(t, srv, "DELETE", "/api/bindings/a/schedules/am", "T", ""); code != 200 {
		t.Fatal("刪除排程失敗")
	}
	if code, _ := call(t, srv, "POST", "/api/bindings/a/set", "T", `{"key":"auto_approve","value":"true"}`); code != 200 {
		t.Fatal("set 失敗")
	}
	if code, _ := call(t, srv, "POST", "/api/bindings/a/pause", "T", ""); code != 200 {
		t.Fatal("pause 失敗")
	}
	cfg := be.Config()
	if !cfg.Bindings[0].Paused || !cfg.Bindings[0].AutoApprove {
		t.Fatalf("設定應已寫入：%+v", cfg.Bindings[0])
	}
	if code, _ := call(t, srv, "POST", "/api/bindings/a/message", "T", `{"text":"x"}`); code != 409 {
		t.Fatal("暫停中的頻道應 409")
	}
	_, body := call(t, srv, "GET", "/api/audit", "T", "")
	var ev []audit.Event
	_ = json.Unmarshal([]byte(body), &ev)
	acts := ""
	for _, e := range ev {
		acts += e.Kind + ":" + e.Action + ":" + e.Actor + " "
	}
	for _, want := range []string{"api:message:web:", "admin:set:web", "admin:pause:web", "api:schedule_add"} {
		if !strings.Contains(acts, want) {
			t.Errorf("稽核 log 缺 %s：%s", want, acts)
		}
	}
}

func TestDelegateViaAPI(t *testing.T) {
	srv, be := setup(t)
	code, body := call(t, srv, "POST", "/api/tasks", "T", `{"binding":"a","task":"查一下 README","role":"researcher"}`)
	if code != 200 || !strings.Contains(body, `"role":"researcher"`) {
		t.Fatalf("%d %s", code, body)
	}
	var tk tasks.Task
	_ = json.Unmarshal([]byte(body), &tk)
	for i := 0; i < 100 && be.apps["a"].Snapshot().Queued == 0; i++ {
		testingSleep()
	}
	if be.apps["a"].Snapshot().Queued != 1 {
		t.Fatal("任務結果應送回頻道 inbox")
	}
	if _, body := call(t, srv, "GET", "/api/tasks?binding=a", "T", ""); !strings.Contains(body, tk.ID) {
		t.Fatal(body)
	}
	if code, _ := call(t, srv, "POST", "/api/tasks", "T", `{"binding":"nope","task":"x"}`); code != 404 {
		t.Fatal("不存在的頻道應 404")
	}
}
