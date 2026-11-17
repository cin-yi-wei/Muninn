package scheduler

import (
	"errors"
	"os"
	"strings"
	"testing"
)

type recorder struct {
	fires []Fire
	seen  map[string]bool
	err   error
}

func (r *recorder) inject(f Fire) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if r.seen[f.ID] {
		return false, nil
	}
	r.seen[f.ID] = true
	r.fires = append(r.fires, f)
	return true, nil
}

func nolog(string, ...any) {}

func newStore(t *testing.T) *Store { return NewStore(t.TempDir(), "Asia/Taipei") }

func tick(t *testing.T, st *Store, now string, r *recorder) int {
	t.Helper()
	n, err := st.RunOnce(at(now), r.inject, nolog)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAddStartsFromNow(t *testing.T) {
	st := newStore(t)
	// 10 點才加：今天 9 點不算錯過
	if _, err := st.Add(Schedule{Name: "am", Cron: "0 9 * * *", Message: "早安", CatchUp: true}, false, at("2026-11-17 10:00")); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	if tick(t, st, "2026-11-17 10:00", r) != 0 || tick(t, st, "2026-11-18 08:59", r) != 0 {
		t.Fatal("不該提早觸發")
	}
	if tick(t, st, "2026-11-18 09:00", r) != 1 || r.fires[0].ID != "am@20261118T0100Z" || r.fires[0].Schedule.Message != "早安" {
		t.Fatalf("隔天 9 點應觸發：%+v", r.fires)
	}
	if tick(t, st, "2026-11-18 09:00", r) != 0 {
		t.Fatal("同一分鐘不該重複")
	}
	s, _ := st.Get("am")
	if s.LastFired == "" {
		t.Fatal("應記錄上次觸發")
	}
	if _, err := st.Add(Schedule{Name: "am", Cron: "@daily", Message: "x"}, false, at("2026-11-18 10:00")); err == nil {
		t.Fatal("同名不加 replace 應拒絕")
	}
}

func TestOneShotFiresOnceThenRemoved(t *testing.T) {
	st := newStore(t)
	if _, err := st.Add(Schedule{Name: "remind", At: "2026-11-17 15:00", Message: "開會"}, false, at("2026-11-17 14:00")); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	tick(t, st, "2026-11-17 14:59", r)
	// 就算 Muninn 剛好停了一小時，單次任務回來也要補
	if tick(t, st, "2026-11-17 16:00", r) != 1 {
		t.Fatal("單次任務應觸發")
	}
	if list, _ := st.List(); len(list) != 0 {
		t.Fatalf("單次任務觸發後應刪除：%+v", list)
	}
	if data, _ := os.ReadFile(st.path); !strings.Contains(string(data), `"schedules": []`) {
		t.Fatalf("清空後應寫成空陣列：%s", data)
	}
	if _, err := st.Add(Schedule{Name: "late", At: "2026-11-17 13:00", Message: "x"}, false, at("2026-11-17 14:00")); err == nil {
		t.Fatal("過去的時間應拒絕")
	}
}

func TestCatchUpOnlyOnceAndSkip(t *testing.T) {
	st := newStore(t)
	now := at("2026-11-14 12:00")
	_, _ = st.Add(Schedule{Name: "catch", Cron: "0 9 * * *", Message: "x", CatchUp: true}, false, now)
	_, _ = st.Add(Schedule{Name: "skip", Cron: "0 9 * * *", Message: "x"}, false, now)
	r := &recorder{}
	// 離線三天後回來
	if tick(t, st, "2026-11-17 10:00", r) != 1 || r.fires[0].ID != "catch@20261117T0100Z" {
		t.Fatalf("只有 catch_up 的補跑一次，而且是最近一次：%+v", r.fires)
	}
	if tick(t, st, "2026-11-17 10:01", r) != 0 {
		t.Fatal("補跑只跑一次")
	}
}

func TestShortDelayStillFires(t *testing.T) {
	st := newStore(t)
	_, _ = st.Add(Schedule{Name: "a", Cron: "0 9 * * *", Message: "x"}, false, at("2026-11-17 08:00"))
	r := &recorder{}
	if tick(t, st, "2026-11-17 09:03", r) != 1 {
		t.Fatal("寬限內的延遲應照樣觸發")
	}
}

func TestInjectFailureRetries(t *testing.T) {
	st := newStore(t)
	_, _ = st.Add(Schedule{Name: "a", Cron: "0 9 * * *", Message: "x"}, false, at("2026-11-17 08:00"))
	r := &recorder{err: errors.New("磁碟滿了")}
	tick(t, st, "2026-11-17 09:00", r)
	r.err = nil
	if tick(t, st, "2026-11-17 09:01", r) != 1 || r.fires[0].ID != "a@20261117T0100Z" {
		t.Fatalf("應重試同一個時間點：%+v", r.fires)
	}
}

func TestPauseResumeAndRemove(t *testing.T) {
	st := newStore(t)
	_, _ = st.Add(Schedule{Name: "a", Cron: "0 9 * * *", Message: "x", CatchUp: true}, false, at("2026-11-17 08:00"))
	if err := st.SetPaused("a", true, at("2026-11-17 08:00")); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	if tick(t, st, "2026-11-17 09:00", r) != 0 {
		t.Fatal("暫停中不該觸發")
	}
	_ = st.SetPaused("a", false, at("2026-11-17 10:00"))
	if tick(t, st, "2026-11-17 10:00", r) != 0 {
		t.Fatal("恢復後不該補跑暫停期間錯過的")
	}
	if err := st.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove("a"); err == nil {
		t.Fatal("刪不存在的應報錯")
	}
}

func TestTimezone(t *testing.T) {
	st := newStore(t)
	_, _ = st.Add(Schedule{Name: "ny", Cron: "30 9 * * *", Message: "x", Timezone: "America/New_York"}, false, at("2026-11-17 20:00"))
	r := &recorder{}
	if tick(t, st, "2026-11-17 22:29", r) != 0 || tick(t, st, "2026-11-17 22:30", r) != 1 {
		t.Fatal("紐約 09:30 = 台北 22:30")
	}
}

func TestValidate(t *testing.T) {
	ok := []Schedule{{Name: "a", Cron: "@daily", Message: "x"}, {Name: "b", At: "2026-11-18T09:00:00+08:00", Message: "x"}}
	for _, s := range ok {
		if err := Validate(&s, "Asia/Taipei"); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	for want, s := range map[string]Schedule{
		"5 個欄位":   {Name: "a", Cron: "0 9 * *", Message: "x"},
		"時區":      {Name: "a", Cron: "0 9 * * *", Message: "x", Timezone: "Mars/Base"},
		"message": {Name: "a", Cron: "0 9 * * *"},
		"名稱":      {Name: "../x", Cron: "0 9 * * *", Message: "x"},
		"擇一":      {Name: "a", Cron: "0 9 * * *", At: "2026-11-18 09:00", Message: "x"},
		"要給":      {Name: "a", Message: "x"},
		"看不懂":     {Name: "a", At: "明天早上", Message: "x"},
	} {
		if err := Validate(&s, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("預期錯誤含 %q，得到 %v", want, err)
		}
	}
}

func TestDescribe(t *testing.T) {
	d := Describe(Schedule{Name: "am", Cron: "0 9 * * *", Message: "早安", Paused: true}, "Asia/Taipei", at("2026-11-17 10:00"))
	if !strings.Contains(d, "11-18 09:00") || !strings.Contains(d, "已暫停") {
		t.Fatal(d)
	}
}
