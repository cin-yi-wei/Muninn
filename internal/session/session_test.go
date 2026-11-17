package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreatePersists(t *testing.T) {
	dir := t.TempDir()
	s1, resume, err := LoadOrCreate(dir)
	if err != nil || resume || !uuidRE.MatchString(s1.ID) {
		t.Fatalf("首次應新建 uuid：%+v resume=%v err=%v", s1, resume, err)
	}
	s2, resume, err := LoadOrCreate(dir)
	if err != nil || !resume || s2.ID != s1.ID {
		t.Fatalf("第二次應沿用並 resume：%+v resume=%v err=%v", s2, resume, err)
	}
}

func TestProjectDirEncoding(t *testing.T) {
	// 對照實際觀察到的 ~/.claude/projects 目錄名稱
	cases := map[string]string{
		"/home/user/project/claude_cron/.channel-agent/control-dc-workspace": "-home-user-project-claude-cron--channel-agent-control-dc-workspace",
		"/home/user/.local/state/muninn-m1/work":                             "-home-user--local-state-muninn-m1-work",
	}
	for in, want := range cases {
		if got := filepath.Base(ProjectDir("/h", in)); got != want {
			t.Errorf("%s → %s，預期 %s", in, got, want)
		}
	}
}

func TestFindLatestAndAdopt(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	pd := ProjectDir(home, work)
	if err := os.MkdirAll(filepath.Join(pd, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), 0o755); err != nil { // subagent 目錄，應忽略
		t.Fatal(err)
	}
	old := filepath.Join(pd, "11111111-1111-4111-8111-111111111111.jsonl")
	newer := filepath.Join(pd, "22222222-2222-4222-8222-222222222222.jsonl")
	for _, p := range []string{old, newer, filepath.Join(pd, "notes.jsonl")} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, past, past)

	c, err := FindLatest(home, work)
	if err != nil || c.ID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("應找到最新的 session：%+v err=%v", c, err)
	}

	state := t.TempDir()
	if err := Adopt(state, c, false); err != nil {
		t.Fatal(err)
	}
	s, resume, err := LoadOrCreate(state)
	if err != nil || !resume || s.ID != c.ID || s.AdoptedFrom != newer {
		t.Fatalf("接手後應 resume 該 session：%+v resume=%v err=%v", s, resume, err)
	}
	if TakeHandoff(state) == "" {
		t.Fatal("接手後應留交接標記")
	}
	if err := ClearHandoff(state); err != nil || TakeHandoff(state) != "" {
		t.Fatal("清除後不應再有交接標記")
	}

	// 已綁別的 session：不加 force 要拒絕
	other := Candidate{ID: "33333333-3333-4333-8333-333333333333"}
	if err := Adopt(state, other, false); err == nil {
		t.Fatal("已綁定時不加 force 應拒絕")
	}
	if err := Adopt(state, other, true); err != nil {
		t.Fatal(err)
	}
}

func TestFindLatestEmpty(t *testing.T) {
	if _, err := FindLatest(t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("沒有 session 時應回錯")
	}
}

func TestExists(t *testing.T) {
	home := t.TempDir()
	id := "44444444-4444-4444-8444-444444444444"
	if Exists(home, id) {
		t.Fatal("還沒有檔案不該存在")
	}
	pd := filepath.Join(home, "projects", "-some-dir")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, id+".jsonl"), []byte("{}\n"), 0o644)
	if !Exists(home, id) || Exists(home, "../../etc/passwd") {
		t.Fatal("Exists 判斷錯誤")
	}
}

func TestRunningMarker(t *testing.T) {
	dir := t.TempDir()
	if Running(dir) {
		t.Fatal("沒標記不該算在跑")
	}
	if err := MarkRunning(dir); err != nil || !Running(dir) {
		t.Fatal("標記後應算在跑")
	}
	ClearRunning(dir)
	if Running(dir) {
		t.Fatal("清掉後不該算在跑")
	}
	_ = os.WriteFile(filepath.Join(dir, "running"), []byte("999999999\n"), 0o644) // 不存在的 pid（崩潰殘留）
	if Running(dir) {
		t.Fatal("殘留標記的 pid 已不在，不該算在跑")
	}
}
