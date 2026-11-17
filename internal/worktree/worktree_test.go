package worktree

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestEnsureCreatesThenReuses(t *testing.T) {
	repo := newRepo(t)
	wt := filepath.Join(t.TempDir(), "feat")
	created, err := Ensure(repo, "feat-x", "main", wt)
	if err != nil || !created {
		t.Fatalf("新分支應建 worktree：created=%v err=%v", created, err)
	}
	if cur, _ := git(wt, "rev-parse", "--abbrev-ref", "HEAD"); cur != "feat-x" {
		t.Fatalf("分支 %q", cur)
	}
	created, err = Ensure(repo, "feat-x", "main", wt)
	if err != nil || created {
		t.Fatalf("已存在應直接沿用：created=%v err=%v", created, err)
	}
}

func TestEnsureExistingBranch(t *testing.T) {
	repo := newRepo(t)
	if _, err := git(repo, "branch", "dev"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "dev")
	if created, err := Ensure(repo, "dev", "", wt); err != nil || !created {
		t.Fatalf("既有分支應建 worktree：%v %v", created, err)
	}
}

func TestEnsureRefusesWrongBranchAndForeignDir(t *testing.T) {
	repo := newRepo(t)
	wt := filepath.Join(t.TempDir(), "w")
	if _, err := Ensure(repo, "a", "main", wt); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(repo, "b", "main", wt); err == nil || !strings.Contains(err.Error(), "不會自動切換") {
		t.Fatalf("分支不對應拒絕：%v", err)
	}
	other := newRepo(t)
	if _, err := Ensure(repo, "main", "", other); err == nil || !strings.Contains(err.Error(), "不屬於") {
		t.Fatalf("別的 repo 應拒絕：%v", err)
	}
	if _, err := Ensure(repo, "main", "", t.TempDir()); err == nil {
		t.Fatal("非 git 目錄應拒絕")
	}
}

func TestEnsureInPlaceRepo(t *testing.T) {
	repo := newRepo(t)
	if _, err := Ensure(repo, "main", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(repo, "other", "", ""); err == nil {
		t.Fatal("直接在 repo 工作但分支不對應拒絕")
	}
}
