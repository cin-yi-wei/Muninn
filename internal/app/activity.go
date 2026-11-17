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
	steps  []string
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
	step := shortTool(act.Tool, act.Input)
	if wd := strings.TrimRight(f.a.Workdir, "/"); wd != "" {
		step = strings.ReplaceAll(step, wd+"/", "")
	}
	f.steps = append(f.steps, step)
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
	ids := append([]string(nil), f.msgIDs...)
	sent := append([]string(nil), f.sent...)
	f.mu.Unlock()

	pages := paginate(steps)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for i, p := range pages {
		text := renderPage(pageHeader(i, len(pages), p, summary), steps[p[0]:p[1]])
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
	n := len(f.steps)
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
		l := len([]rune(s)) + 3 // 「\n• 」
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
func pageHeader(i, total int, p [2]int, summary string) string {
	rng := fmt.Sprintf("步驟 %d–%d", p[0]+1, p[1])
	switch {
	case i < total-1:
		return "📜 " + rng
	case summary != "" && total > 1:
		return summary + "（" + rng + "）"
	case summary != "":
		return summary
	case total > 1:
		return fmt.Sprintf("⏳ **工作中**（步驟 %d 起）", p[0]+1)
	default:
		return "⏳ **工作中**"
	}
}

func renderPage(header string, steps []string) string {
	var b strings.Builder
	b.WriteString(header)
	for _, s := range steps {
		b.WriteString("\n• " + s)
	}
	return b.String()
}

// shortTool 把工具呼叫壓成一行。
func shortTool(tool string, input json.RawMessage) string {
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
	if r := []rune(detail); len(r) > 80 {
		detail = string(r[:80]) + "…"
	}
	detail = strings.ReplaceAll(detail, "`", "'")
	if detail == "" {
		return "`" + tool + "`"
	}
	return "`" + tool + "` " + detail
}
