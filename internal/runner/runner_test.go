package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestActivityCallback(t *testing.T) {
	bin, dir := fakeClaude(t, `#!/bin/sh
while IFS= read -r line; do
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"先看看"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}'
  echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a"}}]}}'
  echo '{"type":"result","subtype":"success","result":"好了"}'
done
`)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	var got []string
	ctx := WithActivity(ctx5s(t), func(a Activity) { got = append(got, a.Tool+" "+string(a.Input)) })
	if _, err := r.Ask(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `Bash {"command":"ls"}` || got[1] != `Read {"file_path":"/a"}` {
		t.Fatalf("%q", got)
	}
	// 沒掛 callback 也不能出事
	if _, err := r.Ask(ctx5s(t), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestExitIncludesStderrAndSilent(t *testing.T) {
	bin, dir := fakeClaude(t, `#!/bin/sh
echo "Invalid API key · Please run /login" >&2
exit 1
`)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u", Stderr: io.Discard})
	_, err := r.Ask(ctx5s(t), "x")
	if !errors.Is(err, ErrExited) || !strings.Contains(err.Error(), "Please run /login") {
		t.Fatalf("提前結束的錯誤應帶 stderr 尾巴：%v", err)
	}

	bin, dir = fakeClaude(t, `#!/bin/sh
while IFS= read -r line; do sleep 30; done
`)
	r = New(Config{Bin: bin, Workdir: dir, SessionID: "u", Stderr: io.Discard})
	go r.Ask(context.Background(), "x")
	time.Sleep(300 * time.Millisecond)
	st := r.Status()
	if !st.Busy || st.Silent < 200*time.Millisecond {
		t.Fatalf("執行中沒有事件時 Silent 應持續增加：%+v", st)
	}
	r.Interrupt()
}

// replayClaude 模擬真的 claude 2.1.326（實測 2026-11-17）：
//   - 每則 stdin 訊息先回放成 {"type":"user","isReplay":true,"uuid":<送進來的 uuid>}，再跑那一輪
//   - 訊息含 BG 時，那一輪結束後過一下子，自己多跑一輪（背景任務完成通知），開頭是 task_notification，沒有回放
//   - 訊息含 BUSY 時，先自己跑完一輪通知，才回放並處理這則（模擬訊息送到時 claude 正在處理通知）
const replayClaude = `#!/bin/sh
n=0
echo '{"type":"system","subtype":"init","apiKeySource":"none"}'
while IFS= read -r line; do
  n=$((n+1))
  uuid=$(printf '%s' "$line" | sed -n 's/.*"uuid":"\([^"]*\)".*/\1/p')
  case "$line" in *BUSY*)
    echo '{"type":"system","subtype":"task_notification","task_id":"t0"}'
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"通知那輪"}]}}'
    echo '{"type":"result","subtype":"success","result":"spont-busy","total_cost_usd":0.5}'
  ;; esac
  echo "{\"type\":\"user\",\"isReplay\":true,\"uuid\":\"$uuid\",\"message\":{\"role\":\"user\",\"content\":\"x\"}}"
  case "$line" in *BG*)
    echo '{"type":"system","subtype":"background_tasks_changed","tasks":[{"id":"t1"}]}'
  ;; esac
  echo "{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"turn=$n\",\"total_cost_usd\":0.$n}"
  case "$line" in *BG*)
    sleep 0.3
    echo '{"type":"system","subtype":"task_notification","task_id":"t1"}'
    echo '{"type":"system","subtype":"background_tasks_changed","tasks":[]}'
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"背景"},{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}]}}'
    echo '{"type":"result","subtype":"success","result":"背景做完了","total_cost_usd":0.9}'
  ;; esac
done
`

func TestSpontaneousTurnGoesToCallback(t *testing.T) {
	bin, dir := fakeClaude(t, replayClaude)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	got := make(chan Reply, 4)
	r.SetOnSpontaneous(func(rep Reply) { got <- rep })

	rep, err := r.Ask(ctx5s(t), "派 BG")
	if err != nil || rep.Text != "turn=1" {
		t.Fatalf("第一輪：%+v %v", rep, err)
	}
	if st := r.Status(); st.BackgroundTasks != 1 {
		t.Fatalf("應記到 1 個背景任務：%+v", st)
	}
	if r.CloseIfIdle(context.Background(), 0) {
		t.Fatal("還有背景任務在跑，不該閒置回收")
	}
	select {
	case s := <-got:
		if s.Text != "背景做完了" {
			t.Fatalf("自發回合內容錯了：%+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("背景任務完成的回合沒有送到 callback")
	}
	// 下一則一定要拿到自己的回覆，不能拿到上一輪留下的
	rep, err = r.Ask(ctx5s(t), "下一則")
	if err != nil || rep.Text != "turn=2" {
		t.Fatalf("錯位了：%+v %v", rep, err)
	}
	if st := r.Status(); st.BackgroundTasks != 0 {
		t.Fatalf("背景任務結束後應歸零：%+v", st)
	}
}

func TestMessageArrivesDuringSpontaneousTurn(t *testing.T) {
	bin, dir := fakeClaude(t, replayClaude)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	got := make(chan Reply, 4)
	r.SetOnSpontaneous(func(rep Reply) { got <- rep })
	if _, err := r.Ask(ctx5s(t), "hi"); err != nil {
		t.Fatal(err)
	}
	rep, err := r.Ask(ctx5s(t), "BUSY")
	if err != nil || rep.Text != "turn=2" {
		t.Fatalf("回放之前的 result 屬於自發回合，不能算成這則的回覆：%+v %v", rep, err)
	}
	select {
	case s := <-got:
		if s.Text != "spont-busy" {
			t.Fatalf("%+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("自發回合沒送到 callback")
	}
}

func TestReplayFlagAndUUIDSent(t *testing.T) {
	bin, dir := fakeClaude(t, `#!/bin/sh
echo "$@" >> "$(dirname "$0")/args.log"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$(dirname "$0")/stdin.log"
  echo '{"type":"result","subtype":"success","result":"ok"}'
done
`)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	if _, err := r.Ask(ctx5s(t), "hi"); err != nil {
		t.Fatal(err)
	}
	args := string(must(os.ReadFile(filepath.Join(dir, "args.log"))))
	in := string(must(os.ReadFile(filepath.Join(dir, "stdin.log"))))
	if !strings.Contains(args, "--replay-user-messages") || !strings.Contains(in, `"uuid":"`) {
		t.Fatalf("要開回放並帶 uuid：args=%s stdin=%s", args, in)
	}
}

func TestRateLimitCallbackGetsFullInfo(t *testing.T) {
	bin, dir := fakeClaude(t, `#!/bin/sh
while IFS= read -r line; do
  echo '{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"seven_day","unifiedWindows":{"seven_day":{"utilization":0.32}}}}'
  echo '{"type":"result","subtype":"success","result":"ok"}'
done
`)
	r := New(Config{Bin: bin, Workdir: dir, SessionID: "u"})
	defer r.Close(context.Background())
	var got string
	r.SetOnRateLimit(func(raw json.RawMessage) { got = string(raw) })
	rep, err := r.Ask(ctx5s(t), "x")
	if err != nil || rep.RateLimit != "seven_day" {
		t.Fatalf("%+v %v", rep, err)
	}
	if !strings.Contains(got, `"utilization":0.32`) {
		t.Fatalf("callback 應拿到完整的 rate_limit_info：%q", got)
	}
}
