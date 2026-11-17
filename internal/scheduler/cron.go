package scheduler

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Spec 是解析後的 cron 表達式。每個欄位用 bitmask 表示允許的值。
//
// 語法（標準 5 欄位：分 時 日 月 週）：
//
//   - N  N-M  */S  N-M/S  N/S  以逗號串接
//     月份可用 JAN～DEC，星期可用 SUN～SAT；星期的 7 也代表週日
//     簡寫：@yearly @annually @monthly @weekly @daily @midnight @hourly
//
// 日與週的關係依標準 cron：兩欄都有限制（都不是 *）時，任一符合就算符合；
// 只有一欄有限制時只看那一欄。
type Spec struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

var shortcuts = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
var dowNames = map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}

type fieldDef struct {
	label    string
	min, max int
	names    map[string]int
}

var fields = [5]fieldDef{
	{"分", 0, 59, nil},
	{"時", 0, 23, nil},
	{"日", 1, 31, nil},
	{"月", 1, 12, monthNames},
	{"週", 0, 7, dowNames}, // 7 = 週日，解析後併到 0
}

// Parse 解析 cron 表達式。
func Parse(expr string) (Spec, error) {
	expr = strings.TrimSpace(expr)
	if s, ok := shortcuts[strings.ToLower(expr)]; ok {
		expr = s
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Spec{}, fmt.Errorf("cron 需要 5 個欄位（分 時 日 月 週）或 @daily 這類簡寫，收到 %q", expr)
	}
	var masks [5]uint64
	for i, p := range parts {
		m, err := parseField(p, fields[i])
		if err != nil {
			return Spec{}, fmt.Errorf("%s欄位 %q：%w", fields[i].label, p, err)
		}
		masks[i] = m
	}
	if masks[4]&(1<<7) != 0 { // 週日寫成 7
		masks[4] = masks[4]&^(1<<7) | 1
	}
	return Spec{
		minute: masks[0], hour: masks[1], dom: masks[2], month: masks[3], dow: masks[4],
		domStar: strings.HasPrefix(parts[2], "*"), dowStar: strings.HasPrefix(parts[4], "*"),
	}, nil
}

func parseField(s string, f fieldDef) (uint64, error) {
	var mask uint64
	for _, item := range strings.Split(s, ",") {
		if item == "" {
			return 0, fmt.Errorf("有空的項目")
		}
		rng, step := item, 1
		if i := strings.IndexByte(item, '/'); i >= 0 {
			n, err := strconv.Atoi(item[i+1:])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("間隔 %q 不合法", item[i+1:])
			}
			rng, step = item[:i], n
		}
		lo, hi := f.min, f.max
		if f.label == "週" {
			hi = 6 // * 不含 7，免得週日算兩次
		}
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = value(a, f); err != nil {
				return 0, err
			}
			if hi, err = value(b, f); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("範圍 %q 前大後小", rng)
			}
		default:
			v, err := value(rng, f)
			if err != nil {
				return 0, err
			}
			lo = v
			if step == 1 {
				hi = v // 單一值
			} else {
				hi = f.max // N/S：從 N 開始每 S 一次
			}
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, nil
}

func value(s string, f fieldDef) (int, error) {
	if f.names != nil {
		if v, ok := f.names[strings.ToUpper(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q 不是數字", s)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%d 超出範圍 %d～%d", v, f.min, f.max)
	}
	return v, nil
}

func has(mask uint64, v int) bool { return mask&(1<<uint(v)) != 0 }

// Match 判斷 t（以 t 自己的時區）是否符合。
func (s Spec) Match(t time.Time) bool {
	if !has(s.minute, t.Minute()) || !has(s.hour, t.Hour()) || !has(s.month, int(t.Month())) {
		return false
	}
	return s.dayMatch(t)
}

func (s Spec) dayMatch(t time.Time) bool {
	d, w := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return w
	case s.dowStar:
		return d
	default:
		return d || w // 兩欄都有限制：任一符合
	}
}

// Next 回傳 after 之後（不含 after 那一分鐘）第一個符合的時間點，以 loc 計算。
// 逐日、逐時跳過不可能的區段，不會一分鐘一分鐘掃；最多往後找 5 年，找不到（例如 2 月 30 日）回 false。
func (s Spec) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !has(s.month, int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatch(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !has(s.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if !has(s.minute, t.Minute()) {
			// 同一小時內找下一個允許的分鐘，沒有就進下一小時
			rest := s.minute >> uint(t.Minute())
			if rest == 0 {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			} else {
				t = t.Add(time.Duration(bits.TrailingZeros64(rest)) * time.Minute)
			}
			continue
		}
		return t, true
	}
	return time.Time{}, false
}
