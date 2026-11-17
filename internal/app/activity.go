package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"muninn/internal/runner"
)

// 活動旁白（M5）：claude 跑一輪的過程中，頻道上維持「工作中」訊息，
// 每用一個工具就更新一次（最多每 activityEvery 改一次，避免撞 Discord rate limit），
// 跑完把最後一則改成總結。整輪沒用任何工具（純聊天）就不會出現。
//
// 步驟不略過：一則累積到 activityPageSteps 步或接近字數上限就凍結（標成「步驟 a–b」），
// 往下另開一則接著寫，所以每一步都留在頻道上。

const (
	activityEvery     = 2 * time.Second
	activityPageSteps = 15
	// activityPageChars：一則裡步驟文字的字元上限，留空間給標題，確保整則不超過 Discord 的 2000 字
	activityPageChars = 1800
)

type activityFeed struct {
	a     *App
	start time.Time

	mu     sync.Mutex
	steps  []string // 顯示用的片段；一個工具呼叫可能拆成好幾段（diff 太長時）
	stepNo []int    // 每個片段屬於第幾步（從 1 起）
	n      int      // 工具呼叫次數
	msgIDs []string // 每一頁對應的訊息 id（依序）
	sent   []string // 每一頁最後一次送出的內容，沒變就不重送
	dirty  bool
	closed bool
	wake   chan struct{}
	done   chan struct{}
}

func (a *App) newActivityFeed() *activityFeed {
	f := &activityFeed{a: a, start: time.Now(), wake: make(chan struct{}, 1), done: make(chan struct{})}
	go f.loop()
	return f
}

// add 由 runner 在 Ask 的 goroutine 裡同步呼叫，只記下來、叫醒 loop，不碰網路。
func (f *activityFeed) add(act runner.Activity) {
	f.mu.Lock()
	bullet := "• "
	if act.Parent != "" {
		bullet = "  ↳ " // subagent 做的步驟縮排，跟主 agent 的分開
	}
	entry := bullet + shortTool(act.Tool, act.Input, f.a.Workdir)
	if blk := toolBlock(act.Tool, act.Input); blk != "" {
		entry += "\n" + blk
	}
	f.n++
	for _, piece := range splitEntry(entry, activityEntryChars) {
		f.steps = append(f.steps, piece)
		f.stepNo = append(f.stepNo, f.n)
	}
	f.dirty = true
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *activityFeed) loop() {
	defer close(f.done)
	var last time.Time
	for range f.wake {
		if wait := activityEvery - time.Since(last); wait > 0 {
			time.Sleep(wait) // 節流：這段時間的新步驟會一起顯示
		}
		f.mu.Lock()
		closed := f.closed
		f.mu.Unlock()
		if closed {
			return
		}
		f.flush("")
		last = time.Now()
	}
}

// flush 把目前步驟送出：已有訊息的頁內容有變才改寫，新的頁另開一則。
// summary 非空時代表這輪結束，最後一頁的標題換成總結。
// 只會在 loop goroutine 或 loop 結束後的 finish 裡呼叫，不會並行。
func (f *activityFeed) flush(summary string) {
	f.mu.Lock()
	if !f.dirty || len(f.steps) == 0 {
		f.mu.Unlock()
		return
	}
	f.dirty = false
	steps := append([]string(nil), f.steps...)
	stepNo := append([]int(nil), f.stepNo...)
	ids := append([]string(nil), f.msgIDs...)
	sent := append([]string(nil), f.sent...)
	f.mu.Unlock()

	pages := paginate(steps)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for i, p := range pages {
		rng := [2]int{stepNo[p[0]], stepNo[p[1]-1]}
		text := renderPage(pageHeader(i, len(pages), rng, summary), steps[p[0]:p[1]])
		if i < len(ids) {
			if text == sent[i] {
				continue
			}
			if err := f.a.Prompter.EditMessage(ctx, f.a.ChannelID, ids[i], text); err != nil {
				f.a.logf("activity: 更新訊息失敗: %v", err)
				continue
			}
			sent[i] = text
			continue
		}
		newID, err := f.a.Prompter.SendPrompt(ctx, f.a.ChannelID, text, nil)
		if err != nil {
			f.a.logf("activity: 發訊息失敗: %v", err)
			f.mu.Lock()
			f.dirty = true // 下次叫醒時重試開新的一則
			f.mu.Unlock()
			break
		}
		ids = append(ids, newID)
		sent = append(sent, text)
	}
	f.mu.Lock()
	f.msgIDs, f.sent = ids, sent
	f.mu.Unlock()
}

// finish 停止更新並把最後一則改成總結；status 例如「✅ 完成」「⏹ 已中斷」。
func (f *activityFeed) finish(status string) {
	f.mu.Lock()
	f.closed = true
	n := f.n
	f.dirty = true
	f.mu.Unlock()
	close(f.wake)
	<-f.done
	if n == 0 {
		return
	}
	f.flush(fmt.Sprintf("%s，%d 個步驟，耗時 %s", status, n, time.Since(f.start).Round(time.Second)))
}

// paginate 把步驟由前往後貪婪分頁，回傳每頁的 [起, 迄) 索引。
// 只往後追加步驟時，前面已經滿的頁不會變（那些訊息已凍結）。
func paginate(steps []string) [][2]int {
	var pages [][2]int
	start, chars := 0, 0
	for i, s := range steps {
		l := len([]rune(s)) + 1 // 加上換行
		if i > start && (i-start >= activityPageSteps || chars+l > activityPageChars) {
			pages = append(pages, [2]int{start, i})
			start, chars = i, 0
		}
		chars += l
	}
	if len(steps) > start {
		pages = append(pages, [2]int{start, len(steps)})
	}
	return pages
}

// pageHeader：前面的頁凍結成「步驟 a–b」；最後一頁是「工作中」或總結，多頁時附上範圍。
// steps 是這頁涵蓋的第一步與最後一步（從 1 起）。
func pageHeader(i, total int, steps [2]int, summary string) string {
	rng := fmt.Sprintf("步驟 %d–%d", steps[0], steps[1])
	if steps[0] == steps[1] {
		rng = fmt.Sprintf("步驟 %d", steps[0])
	}
	switch {
	case i < total-1:
		return "📜 " + rng
	case summary != "" && total > 1:
		return summary + "（" + rng + "）"
	case summary != "":
		return summary
	case total > 1:
		return fmt.Sprintf("⏳ **工作中**（步驟 %d 起）", steps[0])
	default:
		return "⏳ **工作中**"
	}
}

func renderPage(header string, entries []string) string {
	return header + "\n" + strings.Join(entries, "\n")
}

// 改檔內容的顯示上限（照 claude_cron 舊版）：每邊最多 diffMaxLines 行、每行 diffMaxCol 字，
// 超過的顯示「… (+N 行)」。內容太長就拆成好幾則訊息，不截斷到看不見。
const (
	diffMaxLines = 80
	diffMaxCol   = 400
	// activityEntryChars：單一片段的字元上限，要比 activityPageChars 小，留空間給標題
	activityEntryChars = 1700
)

// toolBlock 回傳改檔工具要附在步驟後面的程式碼區塊：Edit／MultiEdit 是 ```diff（- 舊、+ 新），
// Write 是寫入的內容。其他工具回空字串。
func toolBlock(tool string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	s := func(mm map[string]any, k string) string { v, _ := mm[k].(string); return v }
	switch tool {
	case "Edit":
		return diffBlock(s(m, "old_string"), s(m, "new_string"))
	case "MultiEdit":
		arr, _ := m["edits"].([]any)
		var blocks []string
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok {
				if b := diffBlock(s(em, "old_string"), s(em, "new_string")); b != "" {
					blocks = append(blocks, b)
				}
			}
		}
		return strings.Join(blocks, "\n")
	case "Write":
		lines := clampLines(s(m, "content"), diffMaxLines, diffMaxCol)
		if len(lines) == 0 {
			return ""
		}
		return "```\n" + strings.Join(lines, "\n") + "\n```"
	}
	return ""
}

func diffBlock(old, new string) string {
	o := clampLines(old, diffMaxLines, diffMaxCol)
	n := clampLines(new, diffMaxLines, diffMaxCol)
	if len(o) == 0 && len(n) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("```diff\n")
	for _, ln := range o {
		b.WriteString("- " + ln + "\n")
	}
	for _, ln := range n {
		b.WriteString("+ " + ln + "\n")
	}
	b.WriteString("```")
	return b.String()
}

// clampLines 切成行並套用行數、行寬上限。內容裡的 ``` 換成 ˋˋˋ，免得提早關掉外層的程式碼區塊。
func clampLines(s string, maxLines, maxCol int) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	// 檔案結尾的換行不算一行，否則區塊尾巴會多一行空白
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	lines := strings.Split(s, "\n")
	var out []string
	for i, ln := range lines {
		if i >= maxLines {
			out = append(out, fmt.Sprintf("… (+%d 行)", len(lines)-maxLines))
			break
		}
		ln = strings.ReplaceAll(ln, "```", "ˋˋˋ")
		if r := []rune(ln); len(r) > maxCol {
			ln = string(r[:maxCol]) + "…"
		}
		out = append(out, ln)
	}
	return out
}

// splitEntry 把超過 max 字元的片段依行拆開；切點落在程式碼區塊裡時，
// 前段補上收尾 ```、後段重開同一種區塊（```diff 或 ```），每段各自都能正常顯示。
func splitEntry(entry string, max int) []string {
	if len([]rune(entry)) <= max {
		return []string{entry}
	}
	var out []string
	var cur []string
	curLen := 0
	fence := "" // 目前所在區塊的開頭（例如 ```diff），空字串＝不在區塊裡
	for _, ln := range strings.Split(entry, "\n") {
		l := len([]rune(ln)) + 1
		if len(cur) > 0 && curLen+l+4 > max { // +4 留給補上的 \n```
			if fence != "" {
				cur = append(cur, "```")
			}
			out = append(out, strings.Join(cur, "\n"))
			cur, curLen = nil, 0
			if fence != "" {
				cur = append(cur, fence)
				curLen = len([]rune(fence)) + 1
			}
		}
		cur = append(cur, ln)
		curLen += l
		if strings.HasPrefix(ln, "```") {
			if fence == "" {
				fence = ln
			} else {
				fence = ""
			}
		}
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}

// shortTool 把工具呼叫壓成一行。workdir 非空時，裡面的工作目錄前綴換成相對路徑
// （要在截斷之前換，否則長路徑的前綴被切掉就換不掉了）。
func shortTool(tool string, input json.RawMessage, workdir string) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	s := func(k string) string { v, _ := m[k].(string); return v }
	var detail string
	switch tool {
	case "Bash":
		detail = s("description")
		if detail == "" {
			detail = s("command")
		}
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		detail = s("file_path")
		if detail == "" {
			detail = s("notebook_path")
		}
	case "Grep", "Glob":
		detail = s("pattern")
	case "WebFetch":
		detail = s("url")
	case "WebSearch":
		detail = s("query")
	case "Agent", "Task":
		detail = s("description")
	case "TodoWrite":
		detail = "更新待辦清單"
	default:
		if strings.HasPrefix(tool, "mcp__muninn__") {
			tool = strings.TrimPrefix(tool, "mcp__muninn__")
			detail = s("name")
		}
	}
	detail = strings.Join(strings.Fields(detail), " ") // 壓掉換行
	if wd := strings.TrimRight(workdir, "/"); wd != "" {
		detail = strings.ReplaceAll(detail, wd+"/", "")
	}
	if r := []rune(detail); len(r) > 80 {
		detail = string(r[:80]) + "…"
	}
	detail = strings.ReplaceAll(detail, "`", "'")
	if detail == "" {
		return "`" + tool + "`"
	}
	return "`" + tool + "` " + detail
}
