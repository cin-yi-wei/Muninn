package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordTailRotate(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	for i := 0; i < 5; i++ {
		l.Record(Event{Kind: "permission", Binding: map[bool]string{true: "a", false: "b"}[i%2 == 0], Action: "Bash", Result: "allow"})
	}
	if got := l.Tail(10, ""); len(got) != 5 || got[0].Time == "" {
		t.Fatalf("%+v", got)
	}
	if got := l.Tail(10, "a"); len(got) != 3 {
		t.Fatalf("依頻道過濾：%d", len(got))
	}
	if got := l.Tail(2, ""); len(got) != 2 {
		t.Fatal("只取最後 n 筆")
	}
	info, _ := os.Stat(l.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("權限應是 600：%v", info.Mode())
	}
	// 輪替
	f, _ := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write(make([]byte, MaxBytes+1))
	f.Close()
	l.Record(Event{Kind: "admin", Action: "pause"})
	if _, err := os.Stat(l.Path + ".1"); err != nil {
		t.Fatal("超過上限應輪替")
	}
	if got := l.Tail(10, ""); len(got) != 1 {
		t.Fatalf("輪替後新檔只有一筆：%d", len(got))
	}
	var nilLog *Log
	nilLog.Record(Event{}) // 不能 panic
}
