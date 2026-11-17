package scheduler

import (
	"strings"
	"testing"
	"time"
)

var tpe, _ = time.LoadLocation("Asia/Taipei")

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, tpe)
	if err != nil {
		panic(err)
	}
	return t
}

func next(t *testing.T, expr, from string) string {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("%q: %v", expr, err)
	}
	n, ok := s.Next(at(from), tpe)
	if !ok {
		return "none"
	}
	return n.Format("2006-01-02 15:04 Mon")
}

func TestNext(t *testing.T) {
	// 2026-11-17 是週二
	cases := []struct{ expr, from, want string }{
		{"0 9 * * *", "2026-11-17 08:59", "2026-11-17 09:00 Tue"},
		{"0 9 * * *", "2026-11-17 09:00", "2026-11-18 09:00 Wed"}, // 不含起點那一分鐘
		{"*/15 9-12 * * 1-5", "2026-11-17 12:46", "2026-11-18 09:00 Wed"},
		{"0,15,30,45 9-12 * * 1-5", "2026-11-17 10:16", "2026-11-17 10:30 Tue"},
		{"0 8 * * 1-5", "2026-11-20 09:00", "2026-11-23 08:00 Mon"}, // 週五之後跳到週一
		{"30 7 * * SAT,SUN", "2026-11-17 00:00", "2026-11-21 07:30 Sat"},
		{"0 0 * * 7", "2026-11-17 00:00", "2026-11-22 00:00 Sun"}, // 7 = 週日
		{"0 12 1 JAN *", "2026-11-17 00:00", "2027-01-01 12:00 Fri"},
		{"0 0 29 2 *", "2026-11-17 00:00", "2028-02-29 00:00 Tue"}, // 閏年
		{"0 0 30 2 *", "2026-11-17 00:00", "none"},                 // 不存在的日期
		{"@daily", "2026-11-17 13:00", "2026-11-18 00:00 Wed"},
		{"@hourly", "2026-11-17 13:05", "2026-11-17 14:00 Tue"},
		{"5/20 * * * *", "2026-11-17 13:06", "2026-11-17 13:25 Tue"}, // N/S：從 5 開始每 20
		// 日與週都有限制：任一符合（標準 cron）。15 號或週一
		{"0 9 15 * 1", "2026-11-17 10:00", "2026-11-23 09:00 Mon"},
		{"0 9 15 * 1", "2026-12-08 10:00", "2026-12-14 09:00 Mon"},
		{"0 9 15 * 1", "2026-12-14 10:00", "2026-12-15 09:00 Tue"},
	}
	for _, c := range cases {
		if got := next(t, c.expr, c.from); got != c.want {
			t.Errorf("%-24q 從 %s → %s，預期 %s", c.expr, c.from, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for expr, want := range map[string]string{
		"0 9 * *":      "5 個欄位",
		"60 * * * *":   "超出範圍",
		"0 24 * * *":   "超出範圍",
		"0 0 0 * *":    "超出範圍",
		"0 0 * 13 *":   "超出範圍",
		"0 0 * * 8":    "超出範圍",
		"*/0 * * * *":  "間隔",
		"5-1 * * * *":  "前大後小",
		"a * * * *":    "不是數字",
		"1,,2 * * * *": "空的項目",
	} {
		_, err := Parse(expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q：預期錯誤含 %q，得到 %v", expr, want, err)
		}
	}
}

func TestMatch(t *testing.T) {
	s, _ := Parse("30 9 * * MON-FRI")
	if !s.Match(at("2026-11-17 09:30")) || s.Match(at("2026-11-22 09:30")) || s.Match(at("2026-11-17 09:31")) {
		t.Fatal("Match 判斷錯誤")
	}
}
