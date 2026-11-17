// Package scheduler 是 Muninn 的定時任務（DESIGN §3.6、M4）。
//
// 每個 binding 一份 schedules.json（放在 binding 的狀態目錄），由 Muninn 擁有：
// 頻道裡的 claude 透過 Muninn 掛給它的 MCP 工具（schedule_add / list / remove / pause / run_now）
// 管理自己頻道的任務，不需要知道自己是哪個 binding、也不用碰設定檔。
//
// 到期時當成使用者在頻道傳了 message，走同一條 inbox 佇列。
package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Schedule 是一個定時任務（含執行狀態）。
type Schedule struct {
	Name     string `json:"name"`
	Cron     string `json:"cron,omitempty"` // 重複任務：見 Parse
	At       string `json:"at,omitempty"`   // 單次任務：RFC3339，觸發後自動刪除
	Message  string `json:"message"`
	Timezone string `json:"timezone,omitempty"` // IANA 時區；空字串用預設時區
	// CatchUp：Muninn 沒在跑時錯過的時間點，回來後補跑一次（只補最近一次）。
	// false：錯過超過 MissGrace 就跳過。單次任務一律補跑。
	CatchUp   bool   `json:"catch_up,omitempty"`
	Paused    bool   `json:"paused,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`

	// 以下是 Muninn 寫的執行狀態
	LastChecked time.Time `json:"last_checked,omitempty"` // 已檢查到的時間點
	LastFired   string    `json:"last_fired,omitempty"`   // 最近一次觸發的排定時間（顯示用）
}

// OneShot 回報是否為單次任務。
func (s Schedule) OneShot() bool { return s.At != "" }

// MissGrace：不補跑的任務，錯過多久以內仍算「準時」（涵蓋檢查間隔與短暫重啟）。
const MissGrace = 5 * time.Minute

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Location 回傳任務實際使用的時區。
func Location(s Schedule, defaultTZ string) (*time.Location, error) {
	tz := s.Timezone
	if tz == "" {
		tz = defaultTZ
	}
	if tz == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("時區 %q 不合法", tz)
	}
	return loc, nil
}

// parseAt 解析單次任務時間：RFC3339，或 "2006-01-02 15:04"（以任務時區解讀）。
func parseAt(at string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", at, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("時間 %q 看不懂，請用 2006-01-02 15:04 或 RFC3339", at)
}

// Validate 檢查任務設定，並把單次任務的時間正規化成 RFC3339。
func Validate(s *Schedule, defaultTZ string) error {
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("名稱 %q 不合法（小寫英數字、. _ -，最多 64 字）", s.Name)
	}
	if strings.TrimSpace(s.Message) == "" {
		return fmt.Errorf("缺 message")
	}
	loc, err := Location(*s, defaultTZ)
	if err != nil {
		return err
	}
	switch {
	case s.Cron != "" && s.At != "":
		return fmt.Errorf("cron 與 at 只能擇一（重複用 cron，單次用 at）")
	case s.Cron != "":
		if _, err := Parse(s.Cron); err != nil {
			return err
		}
	case s.At != "":
		t, err := parseAt(s.At, loc)
		if err != nil {
			return err
		}
		s.At = t.Format(time.RFC3339)
	default:
		return fmt.Errorf("要給 cron（重複）或 at（單次）")
	}
	return nil
}

// Next 回傳任務在 after 之後的下一次觸發時間。
func Next(s Schedule, defaultTZ string, after time.Time) (time.Time, bool) {
	loc, err := Location(s, defaultTZ)
	if err != nil {
		return time.Time{}, false
	}
	if s.OneShot() {
		t, err := parseAt(s.At, loc)
		if err != nil || !t.After(after) {
			return time.Time{}, false
		}
		return t, true
	}
	spec, err := Parse(s.Cron)
	if err != nil {
		return time.Time{}, false
	}
	return spec.Next(after, loc)
}

// Fire 是一次觸發。
type Fire struct {
	Schedule   Schedule
	Occurrence time.Time
	// ID 由任務名稱與排定時間點導出：同一個時間點不論算到幾次都一樣，注入端用它去重。
	ID     string
	Manual bool // run_now 手動觸發
}

// Injector 把一次觸發送進 binding；created=false 表示這個時間點之前就送過了。
type Injector func(f Fire) (created bool, err error)

// Store 是一個 binding 的 schedules.json。所有讀寫都經過它，避免 MCP 工具與排程器互相蓋掉。
type Store struct {
	mu        sync.Mutex
	path      string
	defaultTZ string
}

// NewStore 建立 store；檔案不存在等同沒有任務。
func NewStore(stateDir, defaultTZ string) *Store {
	return &Store{path: filepath.Join(stateDir, "schedules.json"), defaultTZ: defaultTZ}
}

// DefaultTZ 回傳預設時區。
func (st *Store) DefaultTZ() string { return st.defaultTZ }

func (st *Store) load() ([]Schedule, error) {
	data, err := os.ReadFile(st.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Schedules []Schedule `json:"schedules"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("scheduler: %s 內容壞掉: %w", st.path, err)
	}
	return f.Schedules, nil
}

func (st *Store) save(list []Schedule) error {
	if list == nil {
		list = []Schedule{} // 寫成 [] 而不是 null
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, _ := json.MarshalIndent(map[string]any{"schedules": list}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// List 回傳所有任務。
func (st *Store) List() ([]Schedule, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.load()
}

// Add 新增任務；同名已存在時 replace=false 會拒絕。新任務從現在起算，不會補跑過去的時間點。
func (st *Store) Add(s Schedule, replace bool, now time.Time) (Schedule, error) {
	if err := Validate(&s, st.defaultTZ); err != nil {
		return s, err
	}
	if s.OneShot() {
		if t, _ := time.Parse(time.RFC3339, s.At); !t.After(now) {
			return s, fmt.Errorf("時間 %s 已經過了", s.At)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	list, err := st.load()
	if err != nil {
		return s, err
	}
	s.CreatedAt = now.UTC().Format(time.RFC3339)
	s.LastChecked = now
	s.LastFired = ""
	for i := range list {
		if list[i].Name == s.Name {
			if !replace {
				return s, fmt.Errorf("已經有名為 %q 的任務（要覆蓋請設 replace）", s.Name)
			}
			list[i] = s
			return s, st.save(list)
		}
	}
	return s, st.save(append(list, s))
}

// Remove 刪除任務。
func (st *Store) Remove(name string) error {
	return st.update(name, func(list []Schedule, i int) []Schedule { return append(list[:i], list[i+1:]...) })
}

// SetPaused 暫停或恢復任務。恢復時從現在起算，暫停期間錯過的不補跑。
func (st *Store) SetPaused(name string, paused bool, now time.Time) error {
	return st.update(name, func(list []Schedule, i int) []Schedule {
		if list[i].Paused && !paused {
			list[i].LastChecked = now
		}
		list[i].Paused = paused
		return list
	})
}

// Get 依名稱取任務。
func (st *Store) Get(name string) (Schedule, error) {
	list, err := st.List()
	if err != nil {
		return Schedule{}, err
	}
	for _, s := range list {
		if s.Name == name {
			return s, nil
		}
	}
	return Schedule{}, fmt.Errorf("沒有名為 %q 的任務", name)
}

func (st *Store) update(name string, fn func(list []Schedule, i int) []Schedule) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	list, err := st.load()
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].Name == name {
			return st.save(fn(list, i))
		}
	}
	return fmt.Errorf("沒有名為 %q 的任務", name)
}

// RunOnce 檢查所有任務，到期的交給 inject，回傳實際送出的次數。
//
//   - 到期：送出並把檢查點推進到 now；單次任務送出後刪除
//   - 錯過太久（> MissGrace）且不補跑：跳過並推進
//   - 注入失敗：檢查點不動，下一輪會算出同一個時間點再試（ID 相同，不會重複）
func (st *Store) RunOnce(now time.Time, inject Injector, logf func(string, ...any)) (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	list, err := st.load()
	if err != nil {
		return 0, err
	}
	changed, fired := false, 0
	var keep []Schedule
	for _, s := range list {
		if s.Paused {
			keep = append(keep, s)
			continue
		}
		from := s.LastChecked
		if from.IsZero() {
			// 手動加進檔案、沒有檢查點：從現在起算
			s.LastChecked, changed = now, true
			keep = append(keep, s)
			continue
		}
		due, ok := Next(s, st.defaultTZ, from)
		if !ok || due.After(now) {
			keep = append(keep, s)
			continue
		}
		if !s.OneShot() {
			// 錯過好幾次時只看最後一次（補跑也只補一次）
			for {
				n, ok := Next(s, st.defaultTZ, due)
				if !ok || n.After(now) {
					break
				}
				due = n
			}
			if !s.CatchUp && now.Sub(due) > MissGrace {
				logf("scheduler: %s 錯過 %s 且不補跑，跳過", s.Name, due.Format(time.RFC3339))
				s.LastChecked, changed = now, true
				keep = append(keep, s)
				continue
			}
		}
		created, err := inject(Fire{Schedule: s, Occurrence: due, ID: s.Name + "@" + due.UTC().Format("20060102T1504Z")})
		if err != nil {
			logf("scheduler: %s 注入失敗，下一輪重試: %v", s.Name, err)
			keep = append(keep, s)
			continue
		}
		changed = true
		if created {
			fired++
			logf("scheduler: %s 觸發（排定 %s）", s.Name, due.Format(time.RFC3339))
		}
		if s.OneShot() {
			continue // 單次任務跑完就刪
		}
		s.LastChecked, s.LastFired = now, due.Format(time.RFC3339)
		keep = append(keep, s)
	}
	if !changed {
		return fired, nil
	}
	return fired, st.save(keep)
}

// Describe 把任務整理成一行給人看的文字。
func Describe(s Schedule, defaultTZ string, now time.Time) string {
	when := "cron `" + s.Cron + "`"
	if s.OneShot() {
		when = "單次"
	}
	next := "—"
	if n, ok := Next(s, defaultTZ, now); ok {
		next = n.Format("01-02 15:04 MST")
	}
	status := ""
	if s.Paused {
		status = "（已暫停）"
	}
	msg := []rune(strings.ReplaceAll(s.Message, "\n", " "))
	if len(msg) > 40 {
		msg = append(msg[:40], '…')
	}
	return fmt.Sprintf("`%s` %s，下次 %s%s：%s", s.Name, when, next, status, string(msg))
}
