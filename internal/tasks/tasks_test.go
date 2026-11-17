package tasks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"muninn/internal/runner"
)

type fakeAsker struct {
	t      Task
	extra  []string
	block  chan struct{}
	commit bool
}

func (f *fakeAsker) Ask(ctx context.Context, text string) (runner.Reply, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return runner.Reply{}, ctx.Err()
		}
	}
	if f.commit {
		_ = os.WriteFile(filepath.Join(f.t.Worktree, "out.txt"), []byte(text), 0o644)
		_ = exec.Command("git", "-C", f.t.Worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "x").Run()
		_ = exec.Command("git", "-C", f.t.Worktree, "add", "out.txt").Run()
		_ = exec.Command("git", "-C", f.t.Worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "加 out.txt").Run()
	}
	return runner.Reply{Text: "報告：" + text}, nil
}
func (f *fakeAsker) Close(context.Context) error { return nil }

type harness struct {
	m        *Manager
	repo     string
	mu       sync.Mutex
	delivers []string
	askers   []*fakeAsker
	block    chan struct{}
	commit   bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	h := &harness{repo: repo}
	h.m = &Manager{
		StateDir: filepath.Join(t.TempDir(), "tasks"), MaxRunning: 1,
		NewAsker: func(tk Task, extra []string) Asker {
			h.mu.Lock()
			defer h.mu.Unlock()
			a := &fakeAsker{t: tk, extra: extra, block: h.block, commit: h.commit}
			h.askers = append(h.askers, a)
			return a
		},
		Deliver: func(source, id, text string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.delivers = append(h.delivers, source+"|"+id+"|"+text)
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) src() Source { return Source{Binding: "chan", Repo: h.repo, Workdir: h.repo} }

func (h *harness) wait(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ok := cond()
		h.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待逾時")
}

func TestDelegateRunsAndDelivers(t *testing.T) {
	h := newHarness(t)
	h.commit = true
	tk, err := h.m.Delegate(h.src(), DelegateInput{Task: "寫個檔案", Role: "implementer", Level: "develop"})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, func() bool { return len(h.delivers) == 1 })
	d := h.delivers[0]
	if !strings.HasPrefix(d, "chan|"+tk.ID+"-1|") || !strings.Contains(d, "報告：寫個檔案") || !strings.Contains(d, "加 out.txt") {
		t.Fatalf("結果應送回委派者，附報告與新 commit：%s", d)
	}
	got, _ := h.m.Get("chan", tk.ID)
	if got.Status != StatusDone {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(got.Worktree); err == nil {
		t.Fatal("完成後應移除 worktree")
	}
	if out, _ := gitOut(h.repo, "show", got.Branch+":out.txt"); out != "寫個檔案" {
		t.Fatal("分支上應保留成果")
	}
	extra := strings.Join(h.askers[0].extra, " ")
	if !strings.Contains(extra, "角色：implementer") || strings.Contains(extra, "Edit,Write") {
		t.Fatalf("develop 不該停用寫檔工具：%s", extra)
	}
	// 追問：同一個 session、分支上接著做
	if _, err := h.m.Followup("chan", tk.ID, "再改一下"); err != nil {
		t.Fatal(err)
	}
	h.wait(t, func() bool { return len(h.delivers) == 2 })
	if !strings.HasPrefix(h.delivers[1], "chan|"+tk.ID+"-2|") || h.askers[1].t.SessionID != tk.SessionID {
		t.Fatal("追問應用同一個 session，結果另外送回")
	}
}

func TestReadonlyDisablesWriteTools(t *testing.T) {
	h := newHarness(t)
	if _, err := h.m.Delegate(h.src(), DelegateInput{Task: "查一下", Role: "researcher"}); err != nil {
		t.Fatal(err)
	}
	h.wait(t, func() bool { return len(h.delivers) == 1 })
	if extra := strings.Join(h.askers[0].extra, " "); !strings.Contains(extra, "Edit,Write") || !strings.Contains(extra, "readonly") {
		t.Fatalf("readonly 應停用寫檔工具：%s", extra)
	}
}

func TestQueueCancelAndValidation(t *testing.T) {
	h := newHarness(t)
	h.block = make(chan struct{})
	t1, _ := h.m.Delegate(h.src(), DelegateInput{Task: "一"})
	t2, _ := h.m.Delegate(h.src(), DelegateInput{Task: "二"})
	h.wait(t, func() bool { return len(h.askers) == 1 })
	if g, _ := h.m.Get("chan", t2.ID); g.Status != StatusQueued {
		t.Fatalf("超過上限應排隊：%s", g.Status)
	}
	if _, err := h.m.Followup("chan", t1.ID, "x"); err == nil {
		t.Fatal("執行中不能追問")
	}
	if _, err := h.m.Get("other", t1.ID); err == nil {
		t.Fatal("別的頻道看不到")
	}
	if _, err := h.m.Cancel("chan", t1.ID); err != nil {
		t.Fatal(err)
	}
	h.wait(t, func() bool { return len(h.delivers) == 1 && len(h.askers) == 2 })
	if !strings.Contains(h.delivers[0], "已取消") {
		t.Fatal(h.delivers[0])
	}
	close(h.block)
	h.wait(t, func() bool { return len(h.delivers) == 2 })
	for _, in := range []DelegateInput{{Task: ""}, {Task: "x", Role: "wizard"}, {Task: "x", Level: "root"}} {
		if _, err := h.m.Delegate(h.src(), in); err == nil {
			t.Errorf("%+v 應拒絕", in)
		}
	}
	if _, err := h.m.Delegate(Source{Binding: "task:abc", Repo: h.repo, Workdir: h.repo}, DelegateInput{Task: "x"}); err == nil {
		t.Fatal("任務不能再委派")
	}
}

func TestRestartMarksInterrupted(t *testing.T) {
	h := newHarness(t)
	h.block = make(chan struct{})
	tk, _ := h.m.Delegate(h.src(), DelegateInput{Task: "長任務"})
	h.wait(t, func() bool { return len(h.askers) == 1 })
	var notes []string
	m2 := &Manager{StateDir: h.m.StateDir, NewAsker: h.m.NewAsker, Notify: func(s, text string) { notes = append(notes, text) }}
	if err := m2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	g, _ := m2.Get("chan", tk.ID)
	if g.Status != StatusInterrupted {
		t.Fatalf("重啟後執行中的任務應標成中斷：%s", g.Status)
	}
	close(h.block)
	h.wait(t, func() bool { return len(h.delivers) == 1 }) // 等背景的任務收尾，免得清暫存目錄時還在寫
}
