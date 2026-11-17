package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// refreshTimeout 是主動更新最多等多久。
const refreshTimeout = 60 * time.Second

// refreshPrompt 是送給 claude 的極小請求；我們只要它回的 rate_limit_event，回覆內容不重要。
const refreshPrompt = `{"type":"user","message":{"role":"user","content":"reply OK"}}` + "\n"

// ClaudeRefresher 回傳用 claude 執行檔主動拿額度的 Refresher：開一個極小的 haiku 請求，
// 讀到 rate_limit_event 就記下來，讀到 result 或逾時就結束並砍掉整個 process group。
// 工作目錄用暫存目錄，不碰任何頻道的 worktree；不載入設定、MCP，也不留 session。
func ClaudeRefresher(bin string) Refresher {
	if bin == "" {
		bin = "claude"
	}
	return func(ctx context.Context) (Info, error) {
		ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		defer cancel()
		dir, err := os.MkdirTemp("", "muninn-usage-")
		if err != nil {
			return Info{}, err
		}
		defer os.RemoveAll(dir)

		cmd := exec.Command(bin, "-p",
			"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
			"--model", "haiku", "--strict-mcp-config", "--setting-sources", "", "--no-session-persistence")
		cmd.Dir = dir
		// 自成一個 process group：結束時連同子程序一起砍
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = 5 * time.Second
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return Info{}, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return Info{}, err
		}
		if err := cmd.Start(); err != nil {
			return Info{}, fmt.Errorf("啟動 claude 失敗：%w", err)
		}
		// 送完就關 stdin：claude 處理完這一則就會自己結束
		_, _ = stdin.Write([]byte(refreshPrompt))
		_ = stdin.Close()

		var (
			info      Info
			found     bool
			resultMsg string
		)
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
			for sc.Scan() {
				var ev struct {
					Type    string          `json:"type"`
					Info    json.RawMessage `json:"rate_limit_info"`
					IsError bool            `json:"is_error"`
					Result  string          `json:"result"`
				}
				if json.Unmarshal(sc.Bytes(), &ev) != nil {
					continue // 非 JSON 行直接略過
				}
				switch ev.Type {
				case "rate_limit_event":
					var got Info
					if json.Unmarshal(ev.Info, &got) == nil && len(got.UnifiedWindows) > 0 {
						info, found = got, true
					}
				case "result":
					if ev.IsError {
						resultMsg = ev.Result
					}
					return
				}
			}
		}()

		timedOut := false
		select {
		case <-readDone:
		case <-ctx.Done():
			timedOut = true
		}
		// 負 pid = 整個 process group
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-readDone:
		case <-time.After(5 * time.Second):
		}
		_ = cmd.Wait() // 會關掉 stdout，讀取 goroutine 一定會結束
		<-readDone

		if found {
			return info, nil
		}
		reason := strings.TrimSpace(resultMsg)
		if reason == "" {
			reason = lastLine(stderr.String())
		}
		msg := "claude 沒有回額度資料"
		if timedOut {
			msg = fmt.Sprintf("claude %s 內沒有回額度資料（逾時）", refreshTimeout)
			if ctx.Err() == context.Canceled {
				msg = "更新被取消"
			}
		}
		if reason != "" {
			msg += "：" + reason
		}
		return Info{}, fmt.Errorf("%s", msg)
	}
}

// lastLine 取最後一行非空白、限制長度的文字，放進錯誤訊息用。
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if r := []rune(l); len(r) > 200 {
		l = string(r[:200]) + "…"
	}
	return l
}
