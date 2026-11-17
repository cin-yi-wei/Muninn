// Package scheduler 定時把訊息注入 binding（DESIGN §3.6、M4）。
//
// 設定檔沿用 claude_cron 的 triggers.json 格式，欄位與語意一致：
//
//	{"triggers": [{"name", "binding", "cron", "timezone", "message", "catch_up", "enabled", "last_checked", "created_at"}]}
//
// 到期時當成使用者在頻道傳了 message（走同一條 inbox 佇列）。
package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Trigger 是一個定時任務，格式與 claude_cron 相同。
type Trigger struct {
	Name    string `json:"name"`
	Binding string `json:"binding"`
	// Cron 是標準 5 欄位表達式（分 時 日 月 週）。
	Cron string `json:"cron"`
	// Timezone 是 IANA 時區名；空字串用 DefaultTimezone。
	Timezone string `json:"timezone,omitempty"`
	Message  string `json:"message"`
	// CatchUp：排程器離線時錯過的時間點，回來後要不要補跑一次。
	CatchUp bool `json:"catch_up"`
	Enabled bool `json:"enabled"`
	// LastChecked 是上次檢查到的時間點（RFC3339），用來界定「錯過」的起點。
	LastChecked string `json:"last_checked,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// File 是 triggers.json 的內容。
type File struct {
	Triggers []Trigger `json:"triggers"`
}

// DefaultTimezone 是 Timezone 留空時的時區（與 claude_cron 相同）。
const DefaultTimezone = "Asia/Taipei"

// staleSkip：CatchUp=false 時，錯過超過這麼久就不補跑（吞掉、往前推進），
// 避免重啟後對著陳舊的 LastChecked 誤判「剛好錯過」。與 claude_cron 相同。
const staleSkip = 10 * time.Minute

// Fire 是一次到期的觸發。
type Fire struct {
	Trigger Trigger
	// MessageID 由 occurrence（排定的時間點）導出，同一個時間點不論算到幾次都一樣，
	// 注入端用它去重：寫進 inbox 成功但 last_checked 沒寫回去時，下一輪不會重複執行。
	MessageID  string
	Occurrence time.Time
}

// ErrUnknownBinding 由注入端回傳：trigger 指向的 binding 不存在或已暫停。
var ErrUnknownBinding = errors.New("scheduler: binding 不存在或已暫停")

// Injector 把一次觸發送進對應 binding；created=false 表示這個 occurrence 之前就送過了。
type Injector func(f Fire) (created bool, err error)

// Load 讀取 triggers.json；檔案不存在等同沒有任何 trigger。
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("scheduler: 解析 %s: %w", path, err)
	}
	return f, nil
}

// Save 原子寫入 triggers.json。
func Save(path string, f File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate 檢查 trigger 的 cron 與時區是否合法。
func Validate(t Trigger) error {
	if t.Name == "" || t.Binding == "" {
		return fmt.Errorf("trigger 缺 name 或 binding")
	}
	if _, err := ParseCron(t.Cron); err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}
	if _, err := time.LoadLocation(tzOf(t)); err != nil {
		return fmt.Errorf("%s: 時區 %q 不合法: %w", t.Name, t.Timezone, err)
	}
	return nil
}

func tzOf(t Trigger) string {
	if t.Timezone == "" {
		return DefaultTimezone
	}
	return t.Timezone
}

// RunOnce 檢查所有啟用中的 trigger，到期的交給 inject，回傳實際送出的次數。
// 判斷規則與 claude_cron 的 RunSchedulerOnce 相同。
func RunOnce(path string, now time.Time, inject Injector, logf func(string, ...any)) (int, error) {
	f, err := Load(path)
	if err != nil {
		return 0, err
	}
	updates := map[string]string{} // name → 新的 last_checked
	fired := 0
	for _, t := range f.Triggers {
		if !t.Enabled {
			continue
		}
		loc, err := time.LoadLocation(tzOf(t))
		if err != nil {
			logf("scheduler: %s 時區不合法，略過: %v", t.Name, err)
			continue
		}
		spec, err := ParseCron(t.Cron)
		if err != nil {
			logf("scheduler: %s cron 不合法，略過: %v", t.Name, err)
			continue
		}
		// 起算點：last_checked → created_at → 都沒有就從現在起算並寫回。
		// 沒寫回的話每一輪都從「現在」往後找，下一個時間點永遠在未來，手寫的 trigger 會永遠不觸發。
		from, known := now, false
		for _, s := range []string{t.LastChecked, t.CreatedAt} {
			if parsed, err := time.Parse(time.RFC3339, s); err == nil {
				from, known = parsed, true
				break
			}
		}
		if !known {
			updates[t.Name] = now.UTC().Format(time.RFC3339)
			continue
		}
		due, ok := NextOccurrence(spec, loc, from)
		if !ok || due.After(now) {
			continue // 還沒到期
		}
		nowStr := now.UTC().Format(time.RFC3339)
		if !t.CatchUp && now.Sub(due) > staleSkip {
			logf("scheduler: %s 錯過 %s 且不補跑，跳過", t.Name, due.Format(time.RFC3339))
			updates[t.Name] = nowStr
			continue
		}
		created, err := inject(Fire{
			Trigger:    t,
			MessageID:  fmt.Sprintf("sched:%s:%s", t.Name, due.UTC().Format(time.RFC3339)),
			Occurrence: due,
		})
		if errors.Is(err, ErrUnknownBinding) {
			continue // binding 被刪掉或暫停：略過，不讓整批卡住
		}
		if err != nil {
			// 送不進去：last_checked 不推進，下一輪會算出同一個 occurrence 再試
			logf("scheduler: %s 注入失敗: %v", t.Name, err)
			continue
		}
		updates[t.Name] = nowStr
		if created {
			fired++
			logf("scheduler: %s 觸發（排定 %s）", t.Name, due.Format(time.RFC3339))
		}
	}
	if len(updates) == 0 {
		return fired, nil
	}
	// 寫回前重讀一次，只更新 last_checked：這期間有人手動改了 triggers.json（新增、停用）也不會被蓋掉
	latest, err := Load(path)
	if err != nil {
		return fired, err
	}
	for i := range latest.Triggers {
		if v, ok := updates[latest.Triggers[i].Name]; ok {
			latest.Triggers[i].LastChecked = v
		}
	}
	return fired, Save(path, latest)
}
