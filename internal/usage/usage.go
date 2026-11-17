// Package usage 記錄 claude 訂閱額度的最新狀態，給 !usage 指令用。
//
// 資料來源是 claude stream-json 的 rate_limit_event（每一輪都會送）。額度是整個帳號共用的，
// 所以所有頻道共用一個 Store，誰最近跑過一輪就由誰更新；存成檔案，Muninn 重啟後還看得到。
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Window 是一個額度視窗（例如七天、五小時）。
type Window struct {
	Utilization float64 `json:"utilization"` // 0～1
	ResetsAt    int64   `json:"resetsAt"`    // unix 秒
	Status      string  `json:"status"`      // allowed / allowed_warning / rejected
}

// Info 對應 rate_limit_event 的 rate_limit_info。
type Info struct {
	Status                string            `json:"status"`
	ResetsAt              int64             `json:"resetsAt"`
	RateLimitType         string            `json:"rateLimitType"`
	OverageStatus         string            `json:"overageStatus"`
	OverageDisabledReason string            `json:"overageDisabledReason"`
	IsUsingOverage        bool              `json:"isUsingOverage"`
	UnifiedWindows        map[string]Window `json:"unifiedWindows"`
}

type snapshot struct {
	Info      Info      `json:"info"`
	Binding   string    `json:"binding"` // 哪個頻道的那一輪帶回來的
	UpdatedAt time.Time `json:"updated_at"`
}

// RefreshBinding 是主動更新時記下的來源名稱（不是任何頻道）。
const RefreshBinding = "usage-refresh"

// Refresher 主動拿一次最新額度（daemon 注入真的 claude 請求，測試注入假的）。
type Refresher func(ctx context.Context) (Info, error)

// Store 保存最新一筆額度狀態。
type Store struct {
	Path    string
	Loc     *time.Location // 顯示重置時間用；nil 用機器時區
	Refresh Refresher      // 選填：資料太舊時 Report 先用它更新；nil 不更新

	mu  sync.Mutex
	now func() time.Time // 測試用

	fmu      sync.Mutex
	inflight *flight // 進行中的更新；同時多個 !usage 共用這一次
}

// MaxAge 是資料多舊之後 Report 會先更新。
const MaxAge = 30 * time.Minute

type flight struct {
	done chan struct{}
	err  error
}

// Report 產生 !usage 的回覆：資料超過 MaxAge 或還沒有資料時先更新一次。
// 更新失敗就照舊顯示舊資料，最後附一行原因。
func (s *Store) Report(ctx context.Context) string {
	if s.Refresh == nil {
		return s.Text()
	}
	if snap, ok := s.load(); ok && s.clock().Sub(snap.UpdatedAt) < MaxAge {
		return s.Text()
	}
	if err := s.refresh(ctx); err != nil {
		return s.Text() + "\n更新失敗：" + err.Error()
	}
	return s.Text()
}

// refresh 跑一次 Refresh 並寫進紀錄；已經有一次在跑就等它的結果，不重跑。
func (s *Store) refresh(ctx context.Context) error {
	s.fmu.Lock()
	f := s.inflight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		s.inflight = f
		s.fmu.Unlock()
		f.err = s.refreshOnce(ctx)
		s.fmu.Lock()
		s.inflight = nil
		s.fmu.Unlock()
		close(f.done)
		return f.err
	}
	s.fmu.Unlock()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) refreshOnce(ctx context.Context) error {
	info, err := s.Refresh(ctx)
	if err != nil {
		return err
	}
	if len(info.UnifiedWindows) == 0 {
		return fmt.Errorf("claude 回傳的額度資料沒有視窗資訊")
	}
	s.Update(RefreshBinding, info)
	return nil
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Update 記下一筆新的額度狀態。沒有視窗資料的事件（舊版 claude）不覆蓋既有資料。
func (s *Store) Update(binding string, info Info) {
	if len(info.UnifiedWindows) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(snapshot{Info: info, Binding: binding, UpdatedAt: s.clock()}, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.Path), 0o755)
	tmp := s.Path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, s.Path)
	}
}

func (s *Store) load() (snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snap snapshot
	b, err := os.ReadFile(s.Path)
	if err != nil || json.Unmarshal(b, &snap) != nil || len(snap.Info.UnifiedWindows) == 0 {
		return snap, false
	}
	return snap, true
}

var windowNames = map[string]string{
	"five_hour":        "五小時",
	"seven_day":        "七天",
	"seven_day_opus":   "七天（Opus）",
	"seven_day_sonnet": "七天（Sonnet）",
}

var weekdays = []string{"日", "一", "二", "三", "四", "五", "六"}

// Text 產生 !usage 的回覆。
func (s *Store) Text() string {
	snap, ok := s.load()
	if !ok {
		return "📊 還沒有額度資料：claude 跑完任何一輪之後就會有（任何頻道都可以）。"
	}
	loc := s.Loc
	if loc == nil {
		loc = time.Local
	}
	now := s.clock()

	// 視窗由短到長排：先依重置時間，再依名稱
	keys := make([]string, 0, len(snap.Info.UnifiedWindows))
	for k := range snap.Info.UnifiedWindows {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		wi, wj := snap.Info.UnifiedWindows[keys[i]], snap.Info.UnifiedWindows[keys[j]]
		if wi.ResetsAt != wj.ResetsAt {
			return wi.ResetsAt < wj.ResetsAt
		}
		return keys[i] < keys[j]
	})

	var b strings.Builder
	b.WriteString("📊 **claude 訂閱額度**\n")
	for _, k := range keys {
		w := snap.Info.UnifiedWindows[k]
		name := windowNames[k]
		if name == "" {
			name = k
		}
		mark := "🟢"
		switch {
		case strings.HasPrefix(w.Status, "rejected"):
			mark = "⛔"
		case strings.Contains(w.Status, "warning") || w.Utilization >= 0.8:
			mark = "⚠️"
		}
		fmt.Fprintf(&b, "%s %s：已用 **%.0f%%**", mark, name, w.Utilization*100)
		if w.ResetsAt > 0 {
			t := time.Unix(w.ResetsAt, 0).In(loc)
			fmt.Fprintf(&b, "，%s（%s）%s 重置", t.Format("01-02"), weekdays[t.Weekday()], t.Format("15:04"))
			if left := t.Sub(now); left > 0 {
				if left >= 24*time.Hour {
					fmt.Fprintf(&b, "，還有 %.1f 天", left.Hours()/24)
				} else {
					fmt.Fprintf(&b, "，還有 %s", left.Round(time.Minute))
				}
			}
		}
		b.WriteString("\n")
	}
	switch {
	case snap.Info.IsUsingOverage:
		b.WriteString("超額用量：使用中（會另外計費）\n")
	case snap.Info.OverageStatus == "rejected" || snap.Info.OverageDisabledReason != "":
		b.WriteString("超額用量：關閉（額度用完會直接被擋，不會另外收錢）\n")
	}
	fmt.Fprintf(&b, "_資料時間：%s（%s 那一輪帶回來的）_", ago(now.Sub(snap.UpdatedAt)), snap.Binding)
	return b.String()
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "剛剛"
	case d < time.Hour:
		return fmt.Sprintf("%d 分鐘前", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d 小時前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}
