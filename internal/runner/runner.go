// Package runner 管理一個長駐的 headless claude 程序（stream-json 進出）。
//
// 一個 Runner 對應一個頻道 binding：固定一個 session id，程序活著就一直用同一條
// stdin 送訊息；程序掛掉下次 Ask 會用 --resume 把同一個 session 接回來。
package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config 是啟動 claude 程序需要的設定。
type Config struct {
	Bin       string   // claude 執行檔，預設 "claude"
	Workdir   string   // claude 的工作目錄（頻道綁定的 repo）
	SessionID string   // 這個 binding 固定的 session uuid
	Resume    bool     // true：session 已存在，用 --resume 接續；false：用 --session-id 新開
	Model     string   // 選填
	ExtraArgs []string // 額外 flag（例如之後 M3 的 --permission-prompt-tool）
	Env       []string // 選填，覆寫環境變數（nil 時沿用目前程序的環境）
	Stderr    io.Writer
	// SessionExists 選填：每次啟動前問一次 session 是否真的存在，決定用 --resume 還是 --session-id。
	// 光看 Resume / 是否啟動過不可靠：claude 要等第一則訊息才會寫出 session 檔，
	// 在那之前重啟就去 --resume，claude 會回 "No conversation found"，這個 binding 從此卡死。
	SessionExists func() bool
}

// Reply 是一輪對話（一則 user 訊息到 result 事件）的結果。
type Reply struct {
	Text         string  // result 事件的最終文字；沒有時退回 assistant 文字串接
	IsError      bool    // result.is_error
	Subtype      string  // result.subtype，例如 success / error_max_turns
	SessionID    string  // 事件裡回報的 session id
	CostUSD      float64 // 這一輪的估算花費（訂閱制下也會給估算值，不代表實際扣款）
	TotalCostUSD float64 // result.total_cost_usd：同一個程序啟動以來的累計值
	APIKeySource string  // init 事件的 apiKeySource；"none" 代表走 OAuth 訂閱而非 API key
	RateLimit    string  // 最近一次 rate_limit_event 的 rateLimitType（訂閱視窗，例如 seven_day）
}

// event 只解析我們會用到的欄位。
type event struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	SessionID    string          `json:"session_id"`
	APIKeySource string          `json:"apiKeySource"`
	Result       string          `json:"result"`
	IsError      bool            `json:"is_error"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Message      json.RawMessage `json:"message"`
	RateLimit    *struct {
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
	RateLimitRaw json.RawMessage   `json:"-"`        // rate_limit_info 原文，交給 OnRateLimit
	IsReplay     bool              `json:"isReplay"` // --replay-user-messages 回放的 user 訊息
	UUID         string            `json:"uuid"`
	Tasks        []json.RawMessage `json:"tasks"` // system/background_tasks_changed：目前還在跑的背景任務
}

type assistantMessage struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`  // tool_use
		Input json.RawMessage `json:"input"` // tool_use
	} `json:"content"`
}

// Activity 是 claude 一輪對話中途的動作（目前只回報工具呼叫）。
type Activity struct {
	Tool  string
	Input json.RawMessage
}

type activityKey struct{}

// WithActivity 讓這次 Ask 在 claude 每次呼叫工具時回呼 fn（在 Ask 的 goroutine 裡同步呼叫，fn 不要阻塞）。
func WithActivity(ctx context.Context, fn func(Activity)) context.Context {
	return context.WithValue(ctx, activityKey{}, fn)
}

// Report 回報一個動作給 ctx 上掛的 callback（沒掛就什麼都不做）。
func Report(ctx context.Context, a Activity) {
	if fn, ok := ctx.Value(activityKey{}).(func(Activity)); ok && fn != nil {
		fn(a)
	}
}

// ErrExited 表示 claude 程序在一輪對話結束前就退出了。
var ErrExited = errors.New("runner: claude 程序提前結束")

// Runner 序列化地把訊息送進同一個 claude 程序。並發呼叫 Ask 會排隊。
//
// 自發回合：claude 有背景任務（背景 subagent、背景 Bash）時，任務做完它會自己多跑一輪
// （以 system/task_notification 開頭，沒有對應的 stdin 訊息）。讀取 goroutine 一直在讀輸出，
// 靠 --replay-user-messages 回放的 uuid 分辨每一輪是哪則訊息開的：
// 在這則訊息的回放出現之前收到的 result 都屬於自發回合，交給 OnSpontaneous，不會錯算成這則的回覆。
type Runner struct {
	cfg Config

	mu      sync.Mutex // 一次只跑一輪（Ask）
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{} // 程序結束時關閉
	started bool          // 曾經成功啟動過（之後重啟一律 --resume）

	wmu     sync.Mutex
	waiting *waiter // 目前在等回覆的 Ask；nil 表示沒有
	onSpont func(Reply)
	onRate  func(json.RawMessage)

	statMu       sync.Mutex
	apiKeySource string
	lastTotal    float64 // 上一輪 result 的累計花費，用來算單輪差額；重啟程序時歸零
	rateLimit    string

	// 以下不需持有 mu 就能讀，給 !status、閒置回收、!cancel 用
	pid         atomic.Int64 // 目前 claude 程序的 pid（也是 process group id），0 表示沒在跑
	busy        atomic.Bool  // 正在跑一輪（Ask）
	spontBusy   atomic.Bool  // claude 正在跑自發回合
	bgTasks     atomic.Int64 // claude 回報還在跑的背景任務數
	lastUsed    atomic.Int64 // 上一輪結束的時間（unix nano）
	interrupted atomic.Bool  // 這一輪被 Interrupt 中斷
	lastEvent   atomic.Int64 // 最近一次收到 stream-json 事件的時間（unix nano）
	stderrTail  *tailBuffer
}

// waiter 是一次 Ask 等待中的回合：讀取 goroutine 把屬於它的事件送進 ch。
type waiter struct {
	id   string // 送進 stdin 的 uuid，用來對回放
	ch   chan event
	quit chan struct{} // Ask 返回時關閉，讀取 goroutine 就不再送
}

// SetOnRateLimit 設定收到 rate_limit_event 時要交給誰（rate_limit_info 原文）。
// fn 在讀取 goroutine 裡同步呼叫，不要阻塞。
func (r *Runner) SetOnRateLimit(fn func(json.RawMessage)) {
	r.wmu.Lock()
	r.onRate = fn
	r.wmu.Unlock()
}

// SetOnSpontaneous 設定自發回合（不是由 Ask 開的那一輪）的回覆要交給誰。
// fn 在讀取 goroutine 裡同步呼叫，不要阻塞。沒設定時自發回合的回覆只會被丟掉。
func (r *Runner) SetOnSpontaneous(fn func(Reply)) {
	r.wmu.Lock()
	r.onSpont = fn
	r.wmu.Unlock()
}

// tailBuffer 只保留最後 max 個位元組。
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// ErrInterrupted 表示這一輪被使用者中斷（!cancel / !restart）。
var ErrInterrupted = errors.New("runner: 已被中斷")

// New 建立 Runner，但不立刻啟動程序；第一次 Ask 才啟動。
func New(cfg Config) *Runner {
	if cfg.Bin == "" {
		cfg.Bin = "claude"
	}
	return &Runner{cfg: cfg}
}

// args 組出 claude 的命令列參數。
func (r *Runner) args() []string {
	a := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",              // stream-json 輸出在 -p 模式下必須搭配 --verbose
		"--replay-user-messages", // 回放送進去的訊息（帶 uuid），用來分辨每一輪是誰開的
	}
	resume := r.cfg.Resume || r.started
	if r.cfg.SessionExists != nil {
		resume = r.cfg.SessionExists()
	}
	if resume {
		a = append(a, "--resume", r.cfg.SessionID)
	} else {
		a = append(a, "--session-id", r.cfg.SessionID)
	}
	if r.cfg.Model != "" {
		a = append(a, "--model", r.cfg.Model)
	}
	return append(a, r.cfg.ExtraArgs...)
}

// alive 回報程序是否還在跑。呼叫端需持有 mu。
func (r *Runner) alive() bool {
	if r.cmd == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// start 啟動 claude 程序與讀取 stdout 的 goroutine。呼叫端需持有 mu。
func (r *Runner) start() error {
	cmd := exec.Command(r.cfg.Bin, r.args()...)
	cmd.Dir = r.cfg.Workdir
	// 自成一個 process group：砍程序時連同它開的子程序（Bash 工具、MCP server）一起砍，
	// 否則子程序繼續握著 stdout，讀取端永遠等不到 EOF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 主程序結束後最多再等 5 秒讓 pipe 收尾，避免孫程序握住 pipe 讓 Wait 卡死
	cmd.WaitDelay = 5 * time.Second
	if r.cfg.Env != nil {
		cmd.Env = r.cfg.Env
	}
	// stderr 照常輸出，同時留最後一段：程序提前結束時拿來判斷原因（例如登入失效）
	r.stderrTail = &tailBuffer{max: 4096}
	var errOut io.Writer = os.Stderr
	if r.cfg.Stderr != nil {
		errOut = r.cfg.Stderr
	}
	cmd.Stderr = io.MultiWriter(errOut, r.stderrTail)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("runner: 啟動 claude 失敗: %w", err)
	}
	done := make(chan struct{})
	r.statMu.Lock()
	r.lastTotal = 0
	r.statMu.Unlock()
	r.bgTasks.Store(0)
	r.spontBusy.Store(false)
	go func() {
		sc := bufio.NewScanner(stdout)
		// 單一事件可能很大（長回覆、大型 tool_result），把上限拉到 64MB
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		var d dispatcher
		for sc.Scan() {
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue // 非 JSON 行直接略過
			}
			if ev.Type == "rate_limit_event" {
				var raw struct {
					Info json.RawMessage `json:"rate_limit_info"`
				}
				_ = json.Unmarshal(sc.Bytes(), &raw)
				ev.RateLimitRaw = raw.Info
			}
			r.lastEvent.Store(time.Now().UnixNano())
			r.dispatch(&d, ev)
		}
		_ = cmd.Wait()
		r.pid.CompareAndSwap(int64(cmd.Process.Pid), 0)
		r.spontBusy.Store(false)
		r.bgTasks.Store(0)
		close(done)
	}()
	r.cmd, r.stdin, r.done = cmd, stdin, done
	r.pid.Store(int64(cmd.Process.Pid))
	r.started = true
	return nil
}

// dispatcher 是單一程序的讀取狀態，只在讀取 goroutine 裡用。
type dispatcher struct {
	active     *waiter  // 已看到回放、目前這一輪屬於它的 Ask
	replaySeen bool     // 這個程序回放過訊息（舊版 claude 或測試用的假 claude 不回放）
	spontText  []string // 自發回合累積的 assistant 文字
}

// dispatch 把一個事件分給對的回合：Ask 開的那一輪送進它的 channel，其他的算自發回合。
func (r *Runner) dispatch(d *dispatcher, ev event) {
	switch ev.Type {
	case "system":
		switch ev.Subtype {
		case "init":
			if ev.APIKeySource != "" {
				r.statMu.Lock()
				r.apiKeySource = ev.APIKeySource
				r.statMu.Unlock()
			}
		case "background_tasks_changed":
			if ev.Tasks != nil {
				r.bgTasks.Store(int64(len(ev.Tasks)))
			}
		}
	case "rate_limit_event":
		if ev.RateLimit != nil {
			r.statMu.Lock()
			r.rateLimit = ev.RateLimit.RateLimitType
			r.statMu.Unlock()
		}
		r.wmu.Lock()
		onRate := r.onRate
		r.wmu.Unlock()
		if onRate != nil && len(ev.RateLimitRaw) > 0 {
			onRate(ev.RateLimitRaw)
		}
	}

	r.wmu.Lock()
	pending, onSpont := r.waiting, r.onSpont
	r.wmu.Unlock()

	if ev.Type == "user" && ev.IsReplay {
		d.replaySeen = true
		if pending != nil && ev.UUID == pending.id {
			d.active = pending
		}
		return
	}
	target := d.active
	if target == nil && !d.replaySeen {
		target = pending // 不回放的 claude：照舊把下一個 result 當成這則的回覆
	}
	if target != nil {
		select {
		case target.ch <- ev:
		case <-target.quit:
		}
		if ev.Type == "result" {
			d.active = nil
		}
		return
	}

	// 自發回合
	switch ev.Type {
	case "system":
		if ev.Subtype == "task_notification" {
			r.spontBusy.Store(true)
		}
	case "assistant":
		r.spontBusy.Store(true)
		d.spontText = append(d.spontText, assistantTexts(ev, nil)...)
	case "result":
		rep := r.reply(ev, d.spontText)
		d.spontText = nil
		r.spontBusy.Store(false)
		r.lastUsed.Store(time.Now().UnixNano())
		if onSpont != nil {
			onSpont(rep)
		}
	}
}

// assistantTexts 取出 assistant 事件裡的文字；report 不是 nil 時順便回報工具呼叫。
func assistantTexts(ev event, report func(Activity)) []string {
	var am assistantMessage
	if json.Unmarshal(ev.Message, &am) != nil {
		return nil
	}
	var out []string
	for _, c := range am.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			out = append(out, c.Text)
		}
		if c.Type == "tool_use" && report != nil {
			report(Activity{Tool: c.Name, Input: c.Input})
		}
	}
	return out
}

// newUUID 產生 v4 UUID（claude 的 stream-json 輸入只接受 UUID 格式）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Ask 送出一則 user 訊息，阻塞到收到 result 事件為止。
// ctx 取消時會砍掉程序（下次 Ask 會 --resume 接回 session）。
func (r *Runner) Ask(ctx context.Context, text string) (rep Reply, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interrupted.Store(false)
	r.lastEvent.Store(time.Now().UnixNano()) // 這一輪從現在開始算安靜多久
	r.busy.Store(true)
	defer func() {
		r.busy.Store(false)
		r.lastUsed.Store(time.Now().UnixNano())
		if err != nil && r.interrupted.Load() {
			err = ErrInterrupted
		}
	}()

	if !r.alive() {
		if err := r.start(); err != nil {
			return Reply{}, err
		}
	}

	w := &waiter{id: newUUID(), ch: make(chan event, 64), quit: make(chan struct{})}
	r.wmu.Lock()
	r.waiting = w
	r.wmu.Unlock()
	// release 讓讀取 goroutine 不再送事件給這次 Ask；砍程序前一定要先呼叫，
	// 否則讀取 goroutine 可能卡在送 w.ch，等不到程序結束
	release := sync.OnceFunc(func() {
		r.wmu.Lock()
		r.waiting = nil
		r.wmu.Unlock()
		close(w.quit)
	})
	defer release()

	msg := map[string]any{
		"type": "user",
		"uuid": w.id,
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	line, _ := json.Marshal(msg)
	if _, err := r.stdin.Write(append(line, '\n')); err != nil {
		release()
		r.killLocked()
		return Reply{}, fmt.Errorf("runner: 寫入 stdin 失敗: %w", err)
	}

	report := func(a Activity) { Report(ctx, a) }
	var assistantText []string
	for {
		select {
		case <-ctx.Done():
			release()
			r.killLocked()
			return Reply{}, ctx.Err()
		case ev := <-w.ch:
			switch ev.Type {
			case "assistant":
				assistantText = append(assistantText, assistantTexts(ev, report)...)
			case "result":
				return r.reply(ev, assistantText), nil
			}
		case <-r.done:
			// 讀取 goroutine 已經結束，ch 裡可能還有沒拿的事件；先把 result 撈出來
			for {
				select {
				case ev := <-w.ch:
					if ev.Type == "result" {
						return r.reply(ev, assistantText), nil
					}
				default:
					if tail := strings.TrimSpace(r.stderrTail.String()); tail != "" {
						return Reply{}, fmt.Errorf("%w: %s", ErrExited, lastLines(tail, 3))
					}
					return Reply{}, ErrExited
				}
			}
		}
	}
}

// reply 把 result 事件組成 Reply；result 沒文字時退回 assistant 文字串接。
func (r *Runner) reply(ev event, assistantText []string) Reply {
	r.statMu.Lock()
	defer r.statMu.Unlock()
	rep := Reply{
		Text:         ev.Result,
		IsError:      ev.IsError,
		Subtype:      ev.Subtype,
		SessionID:    ev.SessionID,
		CostUSD:      ev.TotalCostUSD - r.lastTotal,
		TotalCostUSD: ev.TotalCostUSD,
		APIKeySource: r.apiKeySource,
		RateLimit:    r.rateLimit,
	}
	if rep.Text == "" {
		rep.Text = strings.Join(assistantText, "\n\n")
	}
	r.lastTotal = ev.TotalCostUSD
	return rep
}

// killLocked 砍掉目前程序並等它收尾。呼叫端需持有 mu。
func (r *Runner) killLocked() {
	if r.cmd == nil {
		return
	}
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
	if r.cmd.Process != nil {
		// 負 pid = 整個 process group
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
	}
	// 呼叫端已經 release 掉等待中的 Ask，讀取 goroutine 不會卡在送事件，直接等它結束
	<-r.done
}

// Close 關閉 stdin 讓 claude 正常結束；ctx 到期則強制砍掉。
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeLocked(ctx)
}

// Interrupt 中斷正在跑的這一輪（砍掉整個 process group）；回傳 false 表示沒有在跑。
// 下一則訊息會用 --resume 接回同一個 session。
func (r *Runner) Interrupt() bool {
	pid := r.pid.Load()
	if !r.busy.Load() || pid == 0 {
		return false
	}
	r.interrupted.Store(true)
	_ = syscall.Kill(-int(pid), syscall.SIGKILL)
	return true
}

// CloseIfIdle 在沒有跑任何一輪、且閒置超過 d 時關掉 claude 程序，回傳是否關了。
// 正在跑或拿不到鎖就跳過（不會為了回收而等）。
func (r *Runner) CloseIfIdle(ctx context.Context, d time.Duration) bool {
	if !r.mu.TryLock() {
		return false
	}
	defer r.mu.Unlock()
	if !r.alive() || time.Since(time.Unix(0, r.lastUsed.Load())) < d {
		return false
	}
	// 背景任務（背景 subagent、背景 Bash）活在 claude 程序裡，關掉就一起沒了，還會收不到它的結果
	if r.spontBusy.Load() || r.bgTasks.Load() > 0 {
		return false
	}
	r.closeLocked(ctx)
	return true
}

// Restart 中斷正在跑的這一輪並關掉程序；下一則訊息會重新啟動並 --resume。
func (r *Runner) Restart(ctx context.Context) error {
	r.Interrupt()
	return r.Close(ctx)
}

// closeLocked 關閉 stdin 讓 claude 正常結束；ctx 到期則強制砍掉。呼叫端需持有 mu。
func (r *Runner) closeLocked(ctx context.Context) error {
	if !r.alive() {
		return nil
	}
	_ = r.stdin.Close()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		r.killLocked()
		return ctx.Err()
	}
}

// Status 是 runner 目前的狀態快照。
type Status struct {
	Alive    bool
	Busy     bool
	PID      int
	IdleFor  time.Duration // 距離上一輪結束多久（從沒跑過為 0）
	RSSBytes int64         // claude 及其子程序（同一個 process group）的常駐記憶體總和
	Silent   time.Duration // 執行中時，距離上一個 stream-json 事件多久（判斷卡住用）

	Spontaneous     bool // claude 正在跑自發回合（背景任務完成後自己開的那一輪）
	BackgroundTasks int  // claude 回報還在跑的背景任務數
}

// Status 回傳狀態快照，不需等目前這一輪跑完。
func (r *Runner) Status() Status {
	st := Status{PID: int(r.pid.Load()), Busy: r.busy.Load(),
		Spontaneous: r.spontBusy.Load(), BackgroundTasks: int(r.bgTasks.Load())}
	if st.Busy {
		if le := r.lastEvent.Load(); le != 0 {
			st.Silent = time.Since(time.Unix(0, le))
		}
	}
	st.Alive = st.PID != 0
	if lu := r.lastUsed.Load(); lu != 0 {
		st.IdleFor = time.Since(time.Unix(0, lu))
	}
	if st.Alive {
		st.RSSBytes = groupRSS(st.PID)
	}
	return st
}

// groupRSS 加總 process group pgid 底下所有程序的 RSS（讀 /proc，非 Linux 回 0）。
func groupRSS(pgid int) int64 {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	page := int64(os.Getpagesize())
	var total int64
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// 第 2 欄（程序名）可能含空白，從最後一個 ')' 之後開始切；之後依序是 state ppid pgrp
		i := strings.LastIndexByte(string(stat), ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat[i+1:]))
		if len(f) < 3 || f[2] != strconv.Itoa(pgid) {
			continue
		}
		statm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
		if err != nil {
			continue
		}
		if sf := strings.Fields(string(statm)); len(sf) >= 2 {
			if pages, err := strconv.ParseInt(sf[1], 10, 64); err == nil {
				total += pages * page
			}
		}
	}
	return total
}
