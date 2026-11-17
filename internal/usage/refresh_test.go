package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude 寫一個假的 claude 執行檔（shell script），body 是 script 內容。
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeRefresherReadsRateLimit(t *testing.T) {
	// 讀到 result 之後就要結束，不能等程序自己退出（這裡故意讓它之後睡很久，連子程序一起）
	argsFile := filepath.Join(t.TempDir(), "args")
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	bin := fakeClaude(t, `echo "$PWD $*" > `+argsFile+`
head -n 1 > `+stdinFile+`
echo 'not json'
echo '{"type":"system","subtype":"init"}'
echo '{"type":"rate_limit_event","rate_limit_info":`+sample+`}'
echo '{"type":"result","subtype":"success","is_error":false}'
sleep 30 &
sleep 30
`)
	refresh := ClaudeRefresher(bin)
	start := time.Now()
	info, err := refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("讀到 result 後要立刻結束，花了 %s", took)
	}
	if w := info.UnifiedWindows["seven_day"]; w.Utilization != 0.32 {
		t.Fatalf("沒解析到額度：%+v", info)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--model haiku", "--strict-mcp-config", "--no-session-persistence"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("參數缺 %q：%s", want, args)
		}
	}
	// 工作目錄是暫存目錄，結束後要清掉
	wd := strings.Fields(string(args))[0]
	if !strings.Contains(filepath.Base(wd), "muninn-usage-") {
		t.Fatalf("工作目錄要是暫存目錄：%s", wd)
	}
	if _, err := os.Stat(wd); !os.IsNotExist(err) {
		t.Fatalf("暫存目錄沒清掉：%s", wd)
	}
	in, _ := os.ReadFile(stdinFile)
	if !strings.Contains(string(in), `"type":"user"`) || !strings.Contains(string(in), "reply OK") {
		t.Fatalf("stdin 要送一則 user 訊息：%s", in)
	}
}

func TestClaudeRefresherNoEvent(t *testing.T) {
	// 沒有 rate_limit_event 就結束：回錯誤，附 stderr 方便判斷原因
	bin := fakeClaude(t, `echo 'Invalid API key' >&2
echo '{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key"}'
`)
	_, err := ClaudeRefresher(bin)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("要回錯誤並帶原因：%v", err)
	}
}

func TestClaudeRefresherTimeout(t *testing.T) {
	// 卡住不回的 claude 要在逾時後被砍掉
	bin := fakeClaude(t, "sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ClaudeRefresher(bin)(ctx)
	if err == nil || !strings.Contains(err.Error(), "逾時") {
		t.Fatalf("要回逾時錯誤：%v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("逾時後沒有砍乾淨，花了 %s", took)
	}
}

func TestClaudeRefresherMissingBin(t *testing.T) {
	_, err := ClaudeRefresher(filepath.Join(t.TempDir(), "nope"))(context.Background())
	if err == nil {
		t.Fatal("找不到執行檔要回錯誤")
	}
}
