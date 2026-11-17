package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude 寫出一個假的 claude：把參數記到 args.log，每讀一行 stdin 就吐出
// init / assistant / result 事件，result 文字帶上第幾輪與程序 pid，用來驗證
// 「同一個程序處理多輪」和「掛掉後用 --resume 重啟」。
func fakeClaude(t *testing.T, script string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "claude")
	if script == "" {
		script = `#!/bin/sh
echo "$@" >> "$(dirname "$0")/args.log"
n=0
echo '{"type":"system","subtype":"init","session_id":"s1","apiKeySource":"none"}'
while IFS= read -r line; do
  n=$((n+1))
  case "$line" in *DIE*) exit 3;; esac
  echo 'not json noise'
  echo '{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"seven_day"}}'
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"思考中"}]}}'
  echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"s1\",\"result\":\"turn=$n pid=$$\",\"total_cost_usd\":0.0$n}"
done
`
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func ctx5s(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAskReusesOneProcess(t *testing.T) {
	bin, dir := fakeClaude(t, "")
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "uuid-1"})
	defer r.Close(context.Background())

	r1, err := r.Ask(ctx5s(t), "你好")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := r.Ask(ctx5s(t), "再一次")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r1.Text, "turn=1 ") || !strings.HasPrefix(r2.Text, "turn=2 ") {
		t.Fatalf("預期同一程序連續兩輪，得到 %q / %q", r1.Text, r2.Text)
	}
	pid := func(s string) string { return s[strings.Index(s, "pid="):] }
	if pid(r1.Text) != pid(r2.Text) {
		t.Fatalf("兩輪應在同一個程序：%q vs %q", r1.Text, r2.Text)
	}
	// 假 claude 第 n 輪回報累計 0.0n，單輪差額應都是 0.01
	if r1.APIKeySource != "none" || r1.RateLimit != "seven_day" || !near(r1.CostUSD, 0.01) || !near(r2.CostUSD, 0.01) || !near(r2.TotalCostUSD, 0.02) {
		t.Fatalf("計費相關欄位沒解析到：%+v", r1)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	if !strings.Contains(string(args), "--session-id uuid-1") || strings.Contains(string(args), "--resume") {
		t.Fatalf("首次啟動應用 --session-id，實際參數：%s", args)
	}
}

func TestRestartsWithResumeAfterCrash(t *testing.T) {
	bin, dir := fakeClaude(t, "")
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "uuid-2"})
	defer r.Close(context.Background())

	if _, err := r.Ask(ctx5s(t), "DIE"); err != ErrExited {
		t.Fatalf("程序中途退出應回 ErrExited，得到 %v", err)
	}
	rep, err := r.Ask(ctx5s(t), "還在嗎")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.Text, "turn=1 ") {
		t.Fatalf("重啟後應是新程序的第一輪，得到 %q", rep.Text)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "--resume uuid-2") {
		t.Fatalf("第二次啟動應用 --resume，實際：%q", lines)
	}
}

func TestResumeConfigUsesResumeFromStart(t *testing.T) {
	bin, dir := fakeClaude(t, "")
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "uuid-3", Resume: true, Model: "m1", ExtraArgs: []string{"--x"}})
	defer r.Close(context.Background())
	if _, err := r.Ask(ctx5s(t), "hi"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	for _, want := range []string{"--resume uuid-3", "--model m1", "--x", "--input-format stream-json", "--output-format stream-json", "--verbose"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("參數缺 %q：%s", want, args)
		}
	}
}

func TestFallsBackToAssistantTextWhenResultEmpty(t *testing.T) {
	bin, dir := fakeClaude(t, `#!/bin/sh
while IFS= read -r line; do
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"第一段"},{"type":"tool_use","name":"Bash"}]}}'
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"第二段"}]}}'
  echo '{"type":"result","subtype":"success","result":""}'
done
`)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	rep, err := r.Ask(ctx5s(t), "x")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Text != "第一段\n\n第二段" {
		t.Fatalf("得到 %q", rep.Text)
	}
}

func TestAskCancelKillsProcess(t *testing.T) {
	bin, dir := fakeClaude(t, `#!/bin/sh
echo "$@" >> "$(dirname "$0")/args.log"
while IFS= read -r line; do sleep 30; done
`)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := r.Ask(ctx, "x"); err != context.DeadlineExceeded {
		t.Fatalf("預期 DeadlineExceeded，得到 %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("取消後沒有及時返回：%s", time.Since(start))
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestInterruptAndResume(t *testing.T) {
	bin, dir := fakeClaude(t, `#!/bin/sh
echo "$@" >> "$(dirname "$0")/args.log"
while IFS= read -r line; do
  case "$line" in *SLOW*) sleep 30;; esac
  echo '{"type":"result","subtype":"success","result":"ok"}'
done
`)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	if r.Interrupt() {
		t.Fatal("沒在跑時 Interrupt 應回 false")
	}
	errc := make(chan error, 1)
	go func() { _, err := r.Ask(context.Background(), "SLOW"); errc <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for !r.Status().Busy && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := r.Status(); !st.Busy || !st.Alive || st.PID == 0 {
		t.Fatalf("跑一輪時狀態應為 busy：%+v", st)
	}
	if !r.Interrupt() {
		t.Fatal("跑一輪時 Interrupt 應回 true")
	}
	select {
	case err := <-errc:
		if err != ErrInterrupted {
			t.Fatalf("應回 ErrInterrupted，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("中斷後沒有及時返回")
	}
	rep, err := r.Ask(ctx5s(t), "hi")
	if err != nil || rep.Text != "ok" {
		t.Fatalf("中斷後下一輪應正常：%+v %v", rep, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	if !strings.Contains(strings.Split(strings.TrimSpace(string(args)), "\n")[1], "--resume u") {
		t.Fatalf("中斷後重啟應 --resume：%s", args)
	}
}

func TestCloseIfIdle(t *testing.T) {
	bin, dir := fakeClaude(t, "")
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	if _, err := r.Ask(ctx5s(t), "hi"); err != nil {
		t.Fatal(err)
	}
	if r.CloseIfIdle(context.Background(), time.Hour) {
		t.Fatal("還沒閒置夠久不該關")
	}
	if st := r.Status(); !st.Alive || st.RSSBytes <= 0 {
		t.Fatalf("程序應活著且讀得到記憶體：%+v", st)
	}
	time.Sleep(50 * time.Millisecond)
	if !r.CloseIfIdle(context.Background(), 10*time.Millisecond) {
		t.Fatal("閒置超過門檻應關掉")
	}
	if r.Status().Alive {
		t.Fatal("關掉後不該還活著")
	}
	rep, err := r.Ask(ctx5s(t), "again")
	if err != nil || !strings.HasPrefix(rep.Text, "turn=1 ") {
		t.Fatalf("回收後下一則應重新啟動：%+v %v", rep, err)
	}
}

func TestSessionExistsDecidesResume(t *testing.T) {
	bin, dir := fakeClaude(t, "")
	exists := false
	// Resume=true 但 session 其實不存在（重啟前從沒收過訊息）：要用 --session-id
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u", Resume: true, SessionExists: func() bool { return exists }})
	defer r.Close(context.Background())
	if _, err := r.Ask(ctx5s(t), "hi"); err != nil {
		t.Fatal(err)
	}
	_ = r.Close(context.Background())
	exists = true
	if _, err := r.Ask(ctx5s(t), "hi"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(must(os.ReadFile(filepath.Join(dir, "args.log"))))), "\n")
	if !strings.Contains(lines[0], "--session-id u") || !strings.Contains(lines[1], "--resume u") {
		t.Fatalf("應依 session 是否存在決定：%q", lines)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
