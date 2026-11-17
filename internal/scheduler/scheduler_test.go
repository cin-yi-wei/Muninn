package scheduler

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var tpe, _ = time.LoadLocation("Asia/Taipei")

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, tpe)
	if err != nil {
		panic(err)
	}
	return t
}

func setup(t *testing.T, triggers ...Trigger) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "triggers.json")
	if err := Save(p, File{Triggers: triggers}); err != nil {
		t.Fatal(err)
	}
	return p
}

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
	if r.seen[f.MessageID] {
		return false, nil
	}
	r.seen[f.MessageID] = true
	r.fires = append(r.fires, f)
	return true, nil
}

func nolog(string, ...any) {}

func TestFiresOnceAtDueTime(t *testing.T) {
	p := setup(t, Trigger{Name: "am", Binding: "b", Cron: "0 9 * * *", Message: "早安", Enabled: true,
		LastChecked: at("2026-11-17 08:59:30").UTC().Format(time.RFC3339)})
	r := &recorder{}
	if n, _ := RunOnce(p, at("2026-11-17 08:59:50"), r.inject, nolog); n != 0 {
		t.Fatal("還沒到 9 點不該觸發")
	}
	if n, err := RunOnce(p, at("2026-11-17 09:00:10"), r.inject, nolog); n != 1 || err != nil {
		t.Fatalf("9 點應觸發一次：%d %v", n, err)
	}
	if r.fires[0].MessageID != "sched:am:2026-11-17T01:00:00Z" || r.fires[0].Trigger.Message != "早安" {
		t.Fatalf("觸發內容不對：%+v", r.fires[0])
	}
	if n, _ := RunOnce(p, at("2026-11-17 09:00:40"), r.inject, nolog); n != 0 {
		t.Fatal("同一個時間點不該再觸發")
	}
	// 與 claude_cron 相同：last_checked 只在觸發（或跳過）時推進
	f, _ := Load(p)
	if f.Triggers[0].LastChecked != at("2026-11-17 09:00:10").UTC().Format(time.RFC3339) {
		t.Fatalf("last_checked 應停在觸發那一輪：%s", f.Triggers[0].LastChecked)
	}
}

func TestCatchUp(t *testing.T) {
	last := at("2026-11-14 12:00:00").UTC().Format(time.RFC3339) // 離線三天
	p := setup(t,
		Trigger{Name: "catch", Binding: "b", Cron: "0 9 * * *", Enabled: true, CatchUp: true, LastChecked: last},
		Trigger{Name: "skip", Binding: "b", Cron: "0 9 * * *", Enabled: true, CatchUp: false, LastChecked: last},
		Trigger{Name: "off", Binding: "b", Cron: "* * * * *", Enabled: false, LastChecked: last},
	)
	r := &recorder{}
	if n, _ := RunOnce(p, at("2026-11-17 10:00:00"), r.inject, nolog); n != 1 || r.fires[0].Trigger.Name != "catch" {
		t.Fatalf("只有 catch_up 的應補跑一次：%+v", r.fires)
	}
	// 補跑只跑一次，不會把錯過的三天各跑一遍
	if n, _ := RunOnce(p, at("2026-11-17 10:00:30"), r.inject, nolog); n != 0 {
		t.Fatal("補跑應只有一次")
	}
}

func TestInjectFailureDoesNotAdvance(t *testing.T) {
	last := at("2026-11-17 08:59:00").UTC().Format(time.RFC3339)
	p := setup(t,
		Trigger{Name: "x", Binding: "b", Cron: "0 9 * * *", Enabled: true, LastChecked: last},
		Trigger{Name: "gone", Binding: "deleted", Cron: "0 9 * * *", Enabled: true, LastChecked: last},
	)
	r := &recorder{err: errors.New("磁碟滿了")}
	_, _ = RunOnce(p, at("2026-11-17 09:00:10"), r.inject, nolog)
	r.err = nil
	unknown := func(f Fire) (bool, error) {
		if f.Trigger.Binding == "deleted" {
			return false, ErrUnknownBinding
		}
		return r.inject(f)
	}
	if n, _ := RunOnce(p, at("2026-11-17 09:00:40"), unknown, nolog); n != 1 {
		t.Fatal("上一輪注入失敗，這一輪應重試同一個時間點")
	}
	f, _ := Load(p)
	if f.Triggers[1].LastChecked != last {
		t.Fatal("binding 不存在時不該推進 last_checked")
	}
}

func TestSaveKeepsConcurrentEdits(t *testing.T) {
	p := setup(t, Trigger{Name: "x", Binding: "b", Cron: "0 9 * * *", Enabled: true,
		LastChecked: at("2026-11-17 08:59:00").UTC().Format(time.RFC3339)})
	inject := func(f Fire) (bool, error) {
		// 模擬觸發途中有人手動在 triggers.json 新增一個 trigger
		cur, _ := Load(p)
		cur.Triggers = append(cur.Triggers, Trigger{Name: "new", Binding: "b", Cron: "0 10 * * *"})
		_ = Save(p, cur)
		return true, nil
	}
	if _, err := RunOnce(p, at("2026-11-17 09:00:10"), inject, nolog); err != nil {
		t.Fatal(err)
	}
	f, _ := Load(p)
	if len(f.Triggers) != 2 || f.Triggers[1].Name != "new" {
		t.Fatalf("寫回 last_checked 時不該蓋掉手動新增的 trigger：%+v", f.Triggers)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(Trigger{Name: "a", Binding: "b", Cron: "0 9 * * *"}); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []Trigger{
		{Name: "a", Binding: "b", Cron: "0 9 * *"},
		{Name: "a", Binding: "b", Cron: "0 9 * * *", Timezone: "Mars/Base"},
		{Binding: "b", Cron: "0 9 * * *"},
	} {
		if Validate(tr) == nil {
			t.Errorf("應判為不合法：%+v", tr)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || len(f.Triggers) != 0 {
		t.Fatalf("檔案不存在應當作沒有 trigger：%+v %v", f, err)
	}
}

func TestTriggerWithoutLastChecked(t *testing.T) {
	// 手寫的 trigger：只有 created_at
	p := setup(t, Trigger{Name: "a", Binding: "b", Cron: "22 17 * * *", Enabled: true,
		CreatedAt: at("2026-11-17 17:20:16").UTC().Format(time.RFC3339)})
	r := &recorder{}
	if n, _ := RunOnce(p, at("2026-11-17 17:22:20"), r.inject, nolog); n != 1 {
		t.Fatal("沒有 last_checked 時應從 created_at 起算並觸發")
	}
	// 什麼時間都沒有：第一輪只記下起算點，到期後的下一輪觸發
	p = setup(t, Trigger{Name: "b", Binding: "b", Cron: "22 17 * * *", Enabled: true})
	r = &recorder{}
	if n, _ := RunOnce(p, at("2026-11-17 17:20:00"), r.inject, nolog); n != 0 {
		t.Fatal("第一輪不該觸發")
	}
	if f, _ := Load(p); f.Triggers[0].LastChecked == "" {
		t.Fatal("第一輪應寫回起算點")
	}
	if n, _ := RunOnce(p, at("2026-11-17 17:22:10"), r.inject, nolog); n != 1 {
		t.Fatal("到期後應觸發")
	}
}
