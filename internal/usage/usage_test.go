package usage

import (
	"encoding/json"
	"path/filepath"
	"strings"
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
