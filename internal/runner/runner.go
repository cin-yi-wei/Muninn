// Package runner 管理一個長駐的 headless claude 程序（stream-json 進出）。
//
// 一個 Runner 對應一個頻道 binding：固定一個 session id，程序活著就一直用同一條
// stdin 送訊息；程序掛掉下次 Ask 會用 --resume 把同一個 session 接回來。
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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
}

type assistantMessage struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// ErrExited 表示 claude 程序在一輪對話結束前就退出了。
var ErrExited = errors.New("runner: claude 程序提前結束")

// Runner 序列化地把訊息送進同一個 claude 程序。並發呼叫 Ask 會排隊。
type Runner struct {
	cfg Config

	mu      sync.Mutex // 一次只跑一輪
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	events  chan event
	done    chan struct{} // 程序結束時關閉
	started bool          // 曾經成功啟動過（之後重啟一律 --resume）

	apiKeySource string
	lastTotal    float64 // 上一輪 result 的累計花費，用來算單輪差額；重啟程序時歸零
	rateLimit    string
}

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
		"--verbose", // stream-json 輸出在 -p 模式下必須搭配 --verbose
	}
	if r.cfg.Resume || r.started {
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
	if r.cfg.Stderr != nil {
		cmd.Stderr = r.cfg.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
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
	events := make(chan event, 64)
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		// 單一事件可能很大（長回覆、大型 tool_result），把上限拉到 64MB
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue // 非 JSON 行直接略過
			}
			events <- ev
		}
		_ = cmd.Wait()
		close(done)
	}()
	r.cmd, r.stdin, r.events, r.done = cmd, stdin, events, done
	r.lastTotal = 0
	r.started = true
	return nil
}

// Ask 送出一則 user 訊息，阻塞到收到 result 事件為止。
// ctx 取消時會砍掉程序（下次 Ask 會 --resume 接回 session）。
func (r *Runner) Ask(ctx context.Context, text string) (Reply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.alive() {
		if err := r.start(); err != nil {
			return Reply{}, err
		}
	}

	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	line, _ := json.Marshal(msg)
	if _, err := r.stdin.Write(append(line, '\n')); err != nil {
		r.killLocked()
		return Reply{}, fmt.Errorf("runner: 寫入 stdin 失敗: %w", err)
	}

	var assistantText []string
	for {
		select {
		case <-ctx.Done():
			r.killLocked()
			return Reply{}, ctx.Err()
		case ev := <-r.events:
			switch ev.Type {
			case "system":
				if ev.Subtype == "init" && ev.APIKeySource != "" {
					r.apiKeySource = ev.APIKeySource
				}
			case "rate_limit_event":
				if ev.RateLimit != nil {
					r.rateLimit = ev.RateLimit.RateLimitType
				}
			case "assistant":
				var am assistantMessage
				if json.Unmarshal(ev.Message, &am) == nil {
					for _, c := range am.Content {
						if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
							assistantText = append(assistantText, c.Text)
						}
					}
				}
			case "result":
				return r.reply(ev, assistantText), nil
			}
		case <-r.done:
			// 程序結束前可能還有沒讀完的事件；先把緩衝裡的 result 撈出來
			for {
				select {
				case ev := <-r.events:
					if ev.Type == "result" {
						return r.reply(ev, assistantText), nil
					}
				default:
					return Reply{}, ErrExited
				}
			}
		}
	}
}

// reply 把 result 事件組成 Reply；result 沒文字時退回 assistant 文字串接。
func (r *Runner) reply(ev event, assistantText []string) Reply {
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
	// 把 events 排空，避免讀取 goroutine 卡在送 channel 而等不到 done
	for {
		select {
		case <-r.events:
		case <-r.done:
			return
		}
	}
}

// Close 關閉 stdin 讓 claude 正常結束；ctx 到期則強制砍掉。
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.alive() {
		return nil
	}
	_ = r.stdin.Close()
	for {
		select {
		case <-r.events:
		case <-r.done:
			return nil
		case <-ctx.Done():
			r.killLocked()
			return ctx.Err()
		}
	}
}
