package usage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 實測 claude 2.1.326 的 rate_limit_event.rate_limit_info（2026-11-17）
const sample = `{"status":"allowed","resetsAt":1795093200,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":0.32,"resetsAt":1795093200,"status":"allowed"},"five_hour":{"utilization":0.875,"resetsAt":1795000000,"status":"allowed_warning"}}}`

func TestUpdatePersistsAndRenders(t *testing.T) {
	var info Info
	if err := json.Unmarshal([]byte(sample), &info); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Asia/Taipei")
	path := filepath.Join(t.TempDir(), "usage.json")
	s := &Store{Path: path, Loc: loc}
	now := time.Unix(1794920400, 0) // 11-17 22:20 台北，距離七天重置 2 天
	s.now = func() time.Time { return now }
	s.Update("project-a", info)

	// 重新載入（模擬 Muninn 重啟）也讀得到
	s2 := &Store{Path: path, Loc: loc}
	s2.now = func() time.Time { return now.Add(10 * time.Minute) }
	txt := s2.Text()
	for _, want := range []string{"七天", "32%", "11-19", "21:00", "2.0 天", "五小時", "88%", "⚠️", "10 分鐘前", "project-a", "超額用量：關閉"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("缺 %q：\n%s", want, txt)
		}
	}
	if strings.Index(txt, "五小時") > strings.Index(txt, "七天") {
		t.Fatalf("短的視窗要排前面：\n%s", txt)
	}
}

func TestNoDataYet(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	if txt := s.Text(); !strings.Contains(txt, "還沒有") {
		t.Fatal(txt)
	}
}

func TestOldFormatOnlyType(t *testing.T) {
	// 只有 rateLimitType、沒有 unifiedWindows 的事件不能蓋掉已經有的完整資料
	var full, thin Info
	_ = json.Unmarshal([]byte(sample), &full)
	_ = json.Unmarshal([]byte(`{"rateLimitType":"seven_day"}`), &thin)
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s.Update("a", full)
	s.Update("b", thin)
	if txt := s.Text(); !strings.Contains(txt, "32%") {
		t.Fatalf("空的事件蓋掉了資料：\n%s", txt)
	}
}

func sampleInfo(t *testing.T) Info {
	t.Helper()
	var info Info
	if err := json.Unmarshal([]byte(sample), &info); err != nil {
		t.Fatal(err)
	}
	return info
}

// countingRefresher 記錄被叫了幾次，回傳 info／err
func countingRefresher(n *atomic.Int32, info Info, err error) Refresher {
	return func(context.Context) (Info, error) {
		n.Add(1)
		return info, err
	}
}

func TestReportFreshDoesNotRefresh(t *testing.T) {
	var n atomic.Int32
	now := time.Unix(1794920400, 0)
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s.now = func() time.Time { return now }
	s.Update("a", sampleInfo(t))
	s.Refresh = countingRefresher(&n, sampleInfo(t), nil)
	now = now.Add(29 * time.Minute)
	txt := s.Report(context.Background())
	if n.Load() != 0 {
		t.Fatalf("資料 29 分鐘還新鮮，不該更新（更新了 %d 次）", n.Load())
	}
	if !strings.Contains(txt, "29 分鐘前") || !strings.Contains(txt, "a 那一輪") {
		t.Fatalf("要顯示原本的資料：\n%s", txt)
	}
}

func TestReportStaleRefreshes(t *testing.T) {
	var n atomic.Int32
	now := time.Unix(1794920400, 0)
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s.now = func() time.Time { return now }
	s.Update("a", sampleInfo(t))
	fresh := sampleInfo(t)
	fresh.UnifiedWindows["seven_day"] = Window{Utilization: 0.5, ResetsAt: 1795093200, Status: "allowed"}
	s.Refresh = countingRefresher(&n, fresh, nil)
	now = now.Add(31 * time.Minute)
	txt := s.Report(context.Background())
	if n.Load() != 1 {
		t.Fatalf("資料超過 30 分鐘要更新一次（更新了 %d 次）", n.Load())
	}
	for _, want := range []string{"50%", "剛剛", RefreshBinding} {
		if !strings.Contains(txt, want) {
			t.Fatalf("缺 %q（要顯示更新後的資料與時間）：\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "更新失敗") {
		t.Fatalf("成功時不該有失敗訊息：\n%s", txt)
	}
}

func TestReportNoDataRefreshes(t *testing.T) {
	var n atomic.Int32
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s.Refresh = countingRefresher(&n, sampleInfo(t), nil)
	txt := s.Report(context.Background())
	if n.Load() != 1 || !strings.Contains(txt, "32%") || !strings.Contains(txt, "剛剛") {
		t.Fatalf("沒資料時要先更新（更新了 %d 次）：\n%s", n.Load(), txt)
	}
}

func TestReportRefreshFailureKeepsOldData(t *testing.T) {
	var n atomic.Int32
	now := time.Unix(1794920400, 0)
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s.now = func() time.Time { return now }
	s.Update("a", sampleInfo(t))
	s.Refresh = countingRefresher(&n, Info{}, errors.New("claude 逾時"))
	now = now.Add(2 * time.Hour)
	txt := s.Report(context.Background())
	for _, want := range []string{"32%", "2 小時前", "a 那一輪", "更新失敗：claude 逾時"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("缺 %q：\n%s", want, txt)
		}
	}
	if !strings.HasSuffix(txt, "更新失敗：claude 逾時") {
		t.Fatalf("失敗原因要附在最後一行：\n%s", txt)
	}

	// 沒有舊資料又更新失敗：照舊說還沒有資料，並附原因
	s2 := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s2.Refresh = countingRefresher(&n, Info{}, errors.New("找不到 claude"))
	if txt := s2.Report(context.Background()); !strings.Contains(txt, "還沒有") || !strings.Contains(txt, "更新失敗：找不到 claude") {
		t.Fatalf("沒資料又失敗：\n%s", txt)
	}
}

func TestReportEmptyInfoIsFailure(t *testing.T) {
	// 拿回來的事件沒有視窗資料（舊版 claude）也算失敗，不能裝作更新成功
	var n atomic.Int32
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s.Refresh = countingRefresher(&n, Info{RateLimitType: "seven_day"}, nil)
	if txt := s.Report(context.Background()); !strings.Contains(txt, "更新失敗") {
		t.Fatalf("空的額度資料要算失敗：\n%s", txt)
	}
}

func TestReportConcurrentRefreshOnce(t *testing.T) {
	var n atomic.Int32
	release := make(chan struct{})
	info := sampleInfo(t)
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	s.Refresh = func(context.Context) (Info, error) {
		n.Add(1)
		<-release
		return info, nil
	}
	const callers = 8
	var wg sync.WaitGroup
	out := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = s.Report(context.Background())
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // 讓其他呼叫都排進來
	close(release)
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("同時 %d 個 !usage 只能更新一次，實際 %d 次", callers, n.Load())
	}
	for i, txt := range out {
		if !strings.Contains(txt, "32%") {
			t.Fatalf("第 %d 個呼叫沒拿到更新後的資料：\n%s", i, txt)
		}
	}
}

func TestReportWithoutRefresher(t *testing.T) {
	// 沒注入 Refresher 時行為跟 Text 一樣
	s := &Store{Path: filepath.Join(t.TempDir(), "usage.json")}
	if txt := s.Report(context.Background()); !strings.Contains(txt, "還沒有") || strings.Contains(txt, "更新失敗") {
		t.Fatal(txt)
	}
}
