package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"muninn/internal/runner"
)

// toolAsker 模擬 claude 一輪用了好幾個工具。
type toolAsker struct {
	tools []runner.Activity
	gap   time.Duration
	err   error
}

func (t toolAsker) Ask(ctx context.Context, _ string) (runner.Reply, error) {
	for _, a := range t.tools {
		runner.Report(ctx, a)
		time.Sleep(t.gap)
	}
	return runner.Reply{Text: "做完了"}, t.err
}

func tool(name, in string) runner.Activity {
	return runner.Activity{Tool: name, Input: json.RawMessage(in)}
}

func TestShortTool(t *testing.T) {
	cases := map[string]runner.Activity{
		"`Bash` 列出檔案":          tool("Bash", `{"command":"ls -la","description":"列出檔案"}`),
		"`Bash` git status":    tool("Bash", `{"command":"git\nstatus"}`),
		"`Read` /a/b.go":       tool("Read", `{"file_path":"/a/b.go"}`),
		"`schedule_add` am-ci": tool("mcp__muninn__schedule_add", `{"name":"am-ci"}`),
		"`SomethingElse`":      tool("SomethingElse", `{}`),
	}
	for want, a := range cases {
		if got := shortTool(a.Tool, a.Input); got != want {
			t.Errorf("得到 %q，預期 %q", got, want)
		}
	}
	if got := shortTool("Bash", json.RawMessage(`{"command":"`+strings.Repeat("x", 200)+"`"+`"}`)); len([]rune(got)) > 100 || strings.Count(got, "`") != 2 {
		t.Errorf("長指令應截斷、反引號應替換：%q", got)
	}
}

func TestRenderStepsKeepsLast(t *testing.T) {
	var steps []string
	for i := 0; i < 12; i++ {
		steps = append(steps, "s")
	}
	if r := renderSteps("h", steps); !strings.Contains(r, "前面還有 4 步") || strings.Count(r, "• ") != activityMaxLines {
		t.Fatal(r)
	}
}

func runOne(t *testing.T, asker Asker, activity bool) (*fakeSender, *App) {
	t.Helper()
	snd := &fakeSender{}
	a := newApp(t, t.TempDir(), asker, snd, nil)
	a.Activity = activity
	a.Workdir = "/repo/wt"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	_ = a.OnMessage(ctx, msgFrom("m1", "做點事"))
	waitFor(t, func() bool {
		for _, s := range snd.snapshot() {
			if s.text == "做完了" {
				return true
			}
		}
		return false
	})
	return snd, a
}

func TestActivityFeedLifecycle(t *testing.T) {
	asker := toolAsker{gap: 900 * time.Millisecond, tools: []runner.Activity{
		tool("Bash", `{"command":"ls","description":"列出檔案"}`),
		tool("Read", `{"file_path":"/repo/wt/internal/a.go"}`),
		tool("Edit", `{"file_path":"/a.go"}`),
		tool("Bash", `{"command":"go test ./..."}`),
	}}
	snd, _ := runOne(t, asker, true)
	out := snd.snapshot()
	if !strings.Contains(out[0].text, "工作中") || !strings.Contains(out[0].text, "列出檔案") {
		t.Fatalf("第一則應是工作中訊息：%q", out[0].text)
	}
	waitFor(t, func() bool {
		e := snd.editsSnapshot()
		return len(e) > 0 && strings.Contains(e[len(e)-1], "✅ 完成，4 個步驟")
	})
	edits := snd.editsSnapshot()
	final := edits[len(edits)-1]
	if !strings.Contains(final, "`Read` internal/a.go") {
		t.Fatalf("工作目錄內的路徑應顯示成相對路徑：%q", final)
	}
	if !strings.HasPrefix(final, "msg0:") || !strings.Contains(final, "go test ./...") {
		t.Fatalf("最後應改寫同一則訊息成總結：%q", final)
	}
	// 節流：4 步、間隔 0.9 秒，更新次數應少於步驟數 + 1（初次發送 + 最後總結）
	if len(edits) > 4 {
		t.Fatalf("更新太頻繁：%d 次", len(edits))
	}
}

func TestActivityOffOrNoTools(t *testing.T) {
	snd, _ := runOne(t, toolAsker{tools: []runner.Activity{tool("Bash", `{"command":"ls"}`)}}, false)
	if len(snd.snapshot()) != 1 || len(snd.editsSnapshot()) != 0 {
		t.Fatal("關閉時不該有工作中訊息")
	}
	snd, _ = runOne(t, toolAsker{}, true)
	time.Sleep(100 * time.Millisecond)
	if len(snd.snapshot()) != 1 || len(snd.editsSnapshot()) != 0 {
		t.Fatal("純聊天（沒用工具）不該有工作中訊息")
	}
}
