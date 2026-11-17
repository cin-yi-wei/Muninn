// Package tasks 是 Muninn 的委派（agent-to-agent）：頻道裡的 claude 透過 delegate 工具
// 把一件事交給另一個獨立的 headless claude 去做，做完結果自動回到原頻道的對話裡。
//
// 每個任務：
//   - 自己一個 git worktree（分支 muninn-task-<id>，從委派者目前的 commit 開），互不干擾
//   - 自己一個 claude session，帶角色指示與權限等級（readonly：寫檔工具停用；develop：可以改檔與 commit）
//   - 危險操作的權限確認轉到委派者的頻道（由 daemon 接線），沒人能核准時就拒絕
//   - 完成、失敗、取消都會把結果寫進委派者的 inbox，委派者的 claude 會被叫醒處理
//   - 同時執行數有上限，超過就排隊；可以追問（同一個 session 接著問）、取消、查狀態
//
// 狀態存在 <state_dir>/tasks/<id>/task.json，Muninn 重啟時執行中的任務標成「中斷」並通知委派者。
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"muninn/internal/runner"
	"muninn/internal/worktree"
)

// 權限等級。
const (
	LevelReadonly = "readonly"
	LevelDevelop  = "develop"
)

// 任務狀態。
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusDone        = "done"
	StatusFailed      = "failed"
	StatusCanceled    = "canceled"
	StatusInterrupted = "interrupted"
)

// Task 是一個委派任務（持久化在 task.json）。
type Task struct {
	ID        string `json:"id"`
	Source    string `json:"source"` // 委派者的 binding 名稱
	Role      string `json:"role"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Prompt    string `json:"prompt"`
	Status    string `json:"status"`
	SessionID string `json:"session_id"`
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	BaseRev   string `json:"base_rev"`
	Worktree  string `json:"worktree"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	Turns     int    `json:"turns"` // 跑了幾輪（含追問）
	Created   string `json:"created_at"`
	Updated   string `json:"updated_at"`
}

// Active 回報任務是否還在排隊或執行。
func (t Task) Active() bool { return t.Status == StatusQueued || t.Status == StatusRunning }

// Source 是委派者的資訊。
type Source struct {
	Binding string
	Repo    string
	Workdir string // 委派者的工作目錄：任務從這裡目前的 commit 開分支
}

// Asker 是跑 claude 的介面（runner.Runner 實作）。
type Asker interface {
	Ask(ctx context.Context, text string) (runner.Reply, error)
	Close(ctx context.Context) error
}

// Manager 管理所有委派任務。
type Manager struct {
	StateDir    string // <state_dir>/tasks
	MaxRunning  int
	TaskTimeout time.Duration
	// NewAsker 建立跑任務的 claude；extraArgs 由 Manager 依角色與等級組好（含 system prompt、停用工具），
	// 呼叫端再補上 MCP / 權限相關參數。
	NewAsker func(t Task, extraArgs []string) Asker
	// Deliver 把任務結果送回委派者（寫進它的 inbox）；id 用來去重。
	Deliver func(source, id, text string) error
	// Notify 在委派者的頻道貼一行狀態（選填）。
	Notify func(source, text string)
	Logf   func(format string, args ...any)

	mu      sync.Mutex
	tasks   map[string]*Task
	cancels map[string]context.CancelFunc
	sem     chan struct{}
	root    context.Context
}

// Start 載入既有任務；執行中或排隊中的標成中斷（Muninn 重啟了）並通知。
func (m *Manager) Start(ctx context.Context) error {
	m.root = ctx
	if m.MaxRunning <= 0 {
		m.MaxRunning = 3
	}
	if m.TaskTimeout <= 0 {
		m.TaskTimeout = time.Hour
	}
	m.sem = make(chan struct{}, m.MaxRunning)
	m.tasks = map[string]*Task{}
	m.cancels = map[string]context.CancelFunc{}
	if err := os.MkdirAll(m.StateDir, 0o755); err != nil {
		return err
	}
	ents, _ := os.ReadDir(m.StateDir)
	for _, e := range ents {
		data, err := os.ReadFile(filepath.Join(m.StateDir, e.Name(), "task.json"))
		if err != nil {
			continue
		}
		var t Task
		if json.Unmarshal(data, &t) != nil || t.ID == "" {
			continue
		}
		if t.Active() {
			t.Status, t.Error = StatusInterrupted, "Muninn 重啟，任務中斷（可以用 task_followup 接著做）"
			m.save(&t)
			m.notify(t.Source, fmt.Sprintf("⚠️ 委派任務 `%s`（%s）因 Muninn 重啟中斷，可以用 task_followup 接著做", t.ID, t.Title))
		}
		m.tasks[t.ID] = &t
	}
	return nil
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) notify(source, text string) {
	if m.Notify != nil {
		go m.Notify(source, text)
	}
}

func (m *Manager) save(t *Task) {
	t.Updated = time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Join(m.StateDir, t.ID)
	_ = os.MkdirAll(dir, 0o755)
	data, _ := json.MarshalIndent(t, "", "  ")
	tmp := filepath.Join(dir, ".task.json.tmp")
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "task.json"))
	}
}

func newID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("0102") + hex.EncodeToString(b[:])
}

// DelegateInput 是委派參數。
type DelegateInput struct {
	Task  string // 要做的事（完整指示）
	Title string // 簡短標題，選填
	Role  string
	Level string // readonly（預設）／develop
}

// Delegate 建立任務並排進佇列，立刻回傳（不等做完）。
func (m *Manager) Delegate(src Source, in DelegateInput) (Task, error) {
	if strings.TrimSpace(in.Task) == "" {
		return Task{}, errors.New("要給 task（要做的事）")
	}
	role, err := LookupRole(in.Role)
	if err != nil {
		return Task{}, err
	}
	level := strings.ToLower(in.Level)
	if level == "" {
		level = LevelReadonly
	}
	if level != LevelReadonly && level != LevelDevelop {
		return Task{}, fmt.Errorf("level 只能是 readonly 或 develop")
	}
	if strings.HasPrefix(src.Binding, "task:") {
		return Task{}, errors.New("委派任務不能再委派（避免無限遞迴）")
	}
	rev, err := gitOut(src.Workdir, "rev-parse", "HEAD")
	if err != nil {
		return Task{}, fmt.Errorf("讀不到委派者目前的 commit：%w", err)
	}
	title := in.Title
	if title == "" {
		title = firstLine(in.Task, 40)
	}
	id := newID()
	t := &Task{
		ID: id, Source: src.Binding, Role: role.Name, Level: level, Title: title, Prompt: in.Task,
		Status: StatusQueued, SessionID: newUUID(), Repo: src.Repo, Branch: "muninn-task-" + id, BaseRev: rev,
		Worktree: filepath.Join(m.StateDir, id, "wt"), Created: time.Now().UTC().Format(time.RFC3339),
	}
	m.mu.Lock()
	m.tasks[id] = t
	m.save(t)
	cp := *t // 先複製再啟動：goroutine 開始後 t 會被改
	m.mu.Unlock()
	go m.run(id, cp.Prompt)
	m.logf("tasks: %s 委派 %s（%s／%s）", src.Binding, id, role.Name, level)
	return cp, nil
}

// Followup 對同一個任務 session 接著問（任務需已結束）。
func (m *Manager) Followup(source, id, message string) (Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || t.Source != source {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("這個頻道沒有任務 %q", id)
	}
	if t.Active() {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("任務 %s 還在%s，等它結束再追問", id, statusText(t.Status))
	}
	if strings.TrimSpace(message) == "" {
		m.mu.Unlock()
		return Task{}, errors.New("要給 message")
	}
	t.Status, t.Error = StatusQueued, ""
	m.save(t)
	cp := *t
	m.mu.Unlock()
	go m.run(id, message)
	return cp, nil
}

// Cancel 取消任務。
func (m *Manager) Cancel(source, id string) (Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || t.Source != source {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("這個頻道沒有任務 %q", id)
	}
	if !t.Active() {
		m.mu.Unlock()
		return *t, fmt.Errorf("任務 %s 已經%s", id, statusText(t.Status))
	}
	cancel := m.cancels[id]
	t.Status = StatusCanceled
	m.save(t)
	cp := *t
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return cp, nil
}

// Get 取任務（限委派者自己的）。
func (m *Manager) Get(source, id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok || (source != "" && t.Source != source) {
		return Task{}, fmt.Errorf("這個頻道沒有任務 %q", id)
	}
	return *t, nil
}

// Lookup 依 id 取任務（daemon 轉送權限請求用，不檢查來源）。
func (m *Manager) Lookup(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *t, true
}

// List 列出某個委派者的任務（新的在前）。
func (m *Manager) List(source string) []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Task
	for _, t := range m.tasks {
		if source == "" || t.Source == source {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

func (m *Manager) setStatus(id, status string) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return nil
	}
	t.Status = status
	m.save(t)
	return t
}

// run 跑一輪（第一次委派或追問）。
func (m *Manager) run(id, prompt string) {
	// 排隊：等到有空位或被取消
	select {
	case m.sem <- struct{}{}:
	case <-m.root.Done():
		return
	}
	defer func() { <-m.sem }()

	ctx, cancel := context.WithTimeout(m.root, m.TaskTimeout)
	defer cancel()
	m.mu.Lock()
	t := m.tasks[id]
	if t == nil || t.Status == StatusCanceled {
		m.mu.Unlock()
		return
	}
	m.cancels[id] = cancel
	t.Status = StatusRunning
	m.save(t)
	snap := *t
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
	}()

	m.notify(snap.Source, fmt.Sprintf("🛰 委派任務 `%s` 開始（%s／%s）：%s", id, snap.Role, snap.Level, snap.Title))
	reply, err := m.execute(ctx, snap, prompt)

	m.mu.Lock()
	t = m.tasks[id]
	t.Turns++
	switch {
	case t.Status == StatusCanceled:
		t.Error = "已取消"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		t.Status, t.Error = StatusFailed, fmt.Sprintf("超過 %s 沒做完，已中止", m.TaskTimeout)
	case err != nil:
		t.Status, t.Error = StatusFailed, err.Error()
	case reply.IsError:
		t.Status, t.Error = StatusFailed, "claude 回報錯誤："+reply.Text
	default:
		t.Status, t.Result = StatusDone, reply.Text
	}
	m.save(t)
	final := *t
	m.mu.Unlock()

	// 收尾：worktree 有沒提交的改動先 wip commit，移除 worktree（分支保留，追問時再建回來）
	note := ""
	if wip, rerr := worktree.Remove(final.Repo, final.Worktree); rerr != nil {
		note = "\n（worktree 沒移除：" + rerr.Error() + "）"
	} else if wip != "" {
		note = fmt.Sprintf("\n（任務有未提交的改動，已在分支上做 wip commit %s）", wip)
	}
	commits, _ := gitOut(final.Repo, "log", "--oneline", final.BaseRev+".."+final.Branch)
	m.logf("tasks: %s %s", id, final.Status)
	m.notify(final.Source, fmt.Sprintf("%s 委派任務 `%s` %s", statusIcon(final.Status), id, statusText(final.Status)))
	if m.Deliver != nil {
		if err := m.Deliver(final.Source, fmt.Sprintf("%s-%d", id, final.Turns), report(final, commits)+note); err != nil {
			m.logf("tasks: %s 結果送回 %s 失敗: %v", id, final.Source, err)
		}
	}
}

func (m *Manager) execute(ctx context.Context, t Task, prompt string) (runner.Reply, error) {
	// 第一次建 worktree；追問時分支已存在，直接接回
	if _, err := worktree.Ensure(t.Repo, t.Branch, t.BaseRev, t.Worktree); err != nil {
		return runner.Reply{}, fmt.Errorf("建 worktree 失敗：%w", err)
	}
	role, _ := LookupRole(t.Role)
	extra := []string{"--append-system-prompt", systemPrompt(role, t.Level)}
	disallow := "CronCreate,CronDelete,CronList"
	if t.Level == LevelReadonly {
		disallow += ",Edit,Write,MultiEdit,NotebookEdit"
	}
	extra = append(extra, "--disallowedTools", disallow)
	a := m.NewAsker(t, extra)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.Close(cctx)
	}()
	return a.Ask(ctx, prompt)
}

// report 組出送回委派者的訊息。
func report(t Task, commits string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Muninn 委派任務 %s %s｜角色 %s｜%s]\n任務：%s\n", t.ID, statusText(t.Status), t.Role, t.Level, t.Title)
	if commits != "" {
		fmt.Fprintf(&b, "分支 %s 上的新 commit：\n%s\n", t.Branch, commits)
	}
	switch t.Status {
	case StatusDone:
		b.WriteString("\n子任務的報告：\n" + t.Result)
	default:
		b.WriteString("\n原因：" + t.Error)
	}
	b.WriteString("\n\n（這是系統送來的任務結果，不是使用者打的字。請整理重點回報給使用者；要追問用 task_followup。）")
	return b.String()
}

func statusText(s string) string {
	switch s {
	case StatusQueued:
		return "排隊中"
	case StatusRunning:
		return "執行中"
	case StatusDone:
		return "完成"
	case StatusFailed:
		return "失敗"
	case StatusCanceled:
		return "已取消"
	case StatusInterrupted:
		return "中斷"
	}
	return s
}

func statusIcon(s string) string {
	switch s {
	case StatusDone:
		return "✅"
	case StatusCanceled:
		return "⏹"
	}
	return "⚠️"
}

// Describe 把任務整理成一行。
func Describe(t Task) string {
	return fmt.Sprintf("`%s` %s　%s／%s　%s（%s）", t.ID, statusText(t.Status), t.Role, t.Level, t.Title, t.Created[:16])
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
