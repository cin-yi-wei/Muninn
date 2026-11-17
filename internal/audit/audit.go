// Package audit 是 Muninn 的稽核 log：append-only JSONL，記下誰在什麼時候做了什麼。
//
// 記錄：權限決定（放行／拒絕、由誰或什麼規則決定）、頻道管理操作、委派任務、外部 API 呼叫。
// 檔案超過 MaxBytes 時輪替成 .1（只留一份舊的）。
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event 是一筆紀錄。
type Event struct {
	Time    string `json:"time"`
	Kind    string `json:"kind"`              // permission / admin / task / api
	Binding string `json:"binding,omitempty"` // 相關的頻道
	Actor   string `json:"actor,omitempty"`   // 誰：使用者 id、cli、web、api、policy、auto_approve…
	Action  string `json:"action"`            // 做了什麼
	Detail  string `json:"detail,omitempty"`
	Result  string `json:"result,omitempty"` // allow / deny / ok / error…
}

// MaxBytes 是單一 log 檔的上限。
const MaxBytes = 10 << 20

// Log 寫稽核 log。零值（Path 為空）時什麼都不做，方便不需要稽核的地方直接傳 nil。
type Log struct {
	Path string
	mu   sync.Mutex
}

// Record 寫一筆；失敗只會吞掉（稽核不能讓主流程掛掉）。
func (l *Log) Record(e Event) {
	if l == nil || l.Path == "" {
		return
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if r := []rune(e.Detail); len(r) > 500 {
		e.Detail = string(r[:500]) + "…"
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if info, err := os.Stat(l.Path); err == nil && info.Size() > MaxBytes {
		_ = os.Rename(l.Path, l.Path+".1")
	}
	_ = os.MkdirAll(filepath.Dir(l.Path), 0o755)
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// Tail 回傳最後 n 筆（新的在後）；binding 非空時只看那個頻道。
func (l *Log) Tail(n int, binding string) []Event {
	if l == nil || l.Path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.Path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if binding != "" && e.Binding != binding {
			continue
		}
		all = append(all, e)
		if len(all) > n*2 && n > 0 {
			all = all[len(all)-n:] // 邊讀邊裁，避免整個檔都留在記憶體
		}
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}
