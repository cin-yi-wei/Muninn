package app

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestPaginateBySteps(t *testing.T) {
	steps := make([]string, 2*activityPageSteps+3)
	for i := range steps {
		steps[i] = "s"
	}
	pages := paginate(steps)
	if len(pages) != 3 || pages[0] != [2]int{0, activityPageSteps} || pages[2] != [2]int{2 * activityPageSteps, len(steps)} {
		t.Fatalf("應每 %d 步一頁：%v", activityPageSteps, pages)
	}
	// 往後追加步驟不能改到前面已經滿的頁（前面的訊息已經凍結）
	more := append(append([]string(nil), steps...), "x", "y")
	if p2 := paginate(more); p2[0] != pages[0] || p2[1] != pages[1] {
		t.Fatalf("追加步驟後前面的頁變了：%v → %v", pages, p2)
	}
}

func TestPaginateByLength(t *testing.T) {
	long := strings.Repeat("字", 400) // 10 步共 4000 字，必須依字數分頁
	var steps []string
	for i := 0; i < 10; i++ {
		steps = append(steps, long)
	}
	pages := paginate(steps)
	if len(pages) < 2 {
		t.Fatalf("長步驟應依長度分頁：%v", pages)
	}
	for _, p := range pages {
		if r := renderPage("⏳ **工作中**（步驟 999 起）", steps[p[0]:p[1]]); len([]rune(r)) > 2000 {
			t.Fatalf("單則超過 Discord 2000 字：%d", len([]rune(r)))
		}
	}
	if len(paginate(nil)) != 0 {
		t.Fatal("沒有步驟不該有頁")
	}
}

func TestActivityFeedRollsOver(t *testing.T) {
	var tools []runner.Activity
	n := activityPageSteps + 5
	for i := 0; i < n; i++ {
		tools = append(tools, tool("Bash", fmt.Sprintf(`{"command":"step-%02d"}`, i)))
	}
	snd, _ := runOne(t, toolAsker{gap: 20 * time.Millisecond, tools: tools}, true)
	// latest：每則旁白訊息的最後版本（新發的內容，之後被編輯就取代）
	latest := func() map[string]string {
		m := map[string]string{}
		for i, s := range snd.snapshot() {
			if strings.Contains(s.text, "step-") {
				m[fmt.Sprintf("msg%d", i)] = s.text
			}
		}
		for _, e := range snd.editsSnapshot() {
			id, text, _ := strings.Cut(e, ":")
			m[id] = text
		}
		return m
	}
	done := fmt.Sprintf("✅ 完成，%d 個步驟", n)
	waitFor(t, func() bool {
		for _, v := range latest() {
			if strings.Contains(v, done) {
				return true
			}
		}
		return false
	})
	last := latest()
	if len(last) != 2 || last["msg0"] == "" || last["msg1"] == "" {
		t.Fatalf("超過 %d 步應另開第二則（msg0、msg1），實際：%q", activityPageSteps, last)
	}
	all := last["msg0"] + "\n" + last["msg1"]
	for i := 0; i < n; i++ {
		if !strings.Contains(all, fmt.Sprintf("step-%02d", i)) {
			t.Fatalf("第 %d 步不見了：%q", i, all)
		}
	}
	if strings.Contains(all, "前面還有") {
		t.Fatalf("不該再略過步驟：%q", all)
	}
	if !strings.Contains(last["msg0"], fmt.Sprintf("步驟 1–%d", activityPageSteps)) || strings.Contains(last["msg0"], "工作中") {
		t.Fatalf("第一則應凍結成步驟範圍：%q", last["msg0"])
	}
	if !strings.Contains(last["msg1"], "✅ 完成") || !strings.Contains(last["msg1"], fmt.Sprintf("步驟 %d–%d", activityPageSteps+1, n)) {
		t.Fatalf("最後一則應是總結並標出步驟範圍：%q", last["msg1"])
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
