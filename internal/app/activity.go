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

// 活動旁白（M5）：claude 跑一輪的過程中，頻道上維持一則「工作中」訊息，
// 每用一個工具就更新一次（最多每 activityEvery 改一次，避免撞 Discord rate limit），
// 跑完改成一行總結。整輪沒用任何工具（純聊天）就不會出現。

const (
	activityEvery    = 2 * time.Second
	activityMaxLines = 8
)

type activityFeed struct {
	a     *App
	start time.Time

	mu     sync.Mutex
	steps  []string
	msgID  string
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
	f.steps = append(f.steps, shortTool(act.Tool, act.Input))
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
		f.flush("⏳ **工作中**")
		last = time.Now()
	}
}

// flush 把目前步驟送出（第一次新發訊息，之後改寫同一則）。
func (f *activityFeed) flush(header string) {
	f.mu.Lock()
	if !f.dirty || len(f.steps) == 0 {
		f.mu.Unlock()
		return
	}
	f.dirty = false
	text := renderSteps(header, f.steps)
	id := f.msgID
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if id == "" {
		newID, err := f.a.Prompter.SendPrompt(ctx, f.a.ChannelID, text, nil)
		if err != nil {
			f.a.logf("activity: 發訊息失敗: %v", err)
			return
		}
		f.mu.Lock()
		f.msgID = newID
		f.mu.Unlock()
		return
	}
	if err := f.a.Prompter.EditMessage(ctx, f.a.ChannelID, id, text); err != nil {
		f.a.logf("activity: 更新訊息失敗: %v", err)
	}
}

// finish 停止更新並把訊息改成總結；status 例如「完成」「已中斷」。
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

func renderSteps(header string, steps []string) string {
	var b strings.Builder
	b.WriteString(header)
	shown := steps
	if len(steps) > activityMaxLines {
		fmt.Fprintf(&b, "\n…（前面還有 %d 步）", len(steps)-activityMaxLines)
		shown = steps[len(steps)-activityMaxLines:]
	}
	for _, s := range shown {
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
