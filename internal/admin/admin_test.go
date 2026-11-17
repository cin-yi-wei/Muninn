package admin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"muninn/internal/registry"
)

type fakeDiscord struct {
	created, deleted []string
	fail             bool
}

func (f *fakeDiscord) CreateChannel(_ context.Context, guild, name, parent, _ string) (string, error) {
	if f.fail {
		return "", errors.New("no perm")
	}
	f.created = append(f.created, guild+"/"+parent+"/"+name)
	return "999", nil
}
func (f *fakeDiscord) DeleteChannel(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func setup(t *testing.T) (*Admin, *fakeDiscord, string, string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	state := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "muninn.json")
	body := `{"state_dir":"` + state + `","allowed_users":["u"],"discord_guild":"G","discord_category":"C","bindings":[]}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &fakeDiscord{}
	return &Admin{ConfigPath: cfg, Discord: d}, d, repo, state
}

func TestBindCreatesChannelAndWorktree(t *testing.T) {
	a, d, repo, state := setup(t)
	msg, err := a.Bind(context.Background(), BindOptions{Name: "feat-x", Repo: repo, Branch: "feat-x", Base: "main", CreateChannel: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.created) != 1 || d.created[0] != "G/C/feat-x" || !strings.Contains(msg, "<#999>") {
		t.Fatalf("應在指定伺服器、分類建頻道：%v %s", d.created, msg)
	}
	cfg, _ := registry.Load(a.ConfigPath)
	b, ok := cfg.Find("feat-x")
	if !ok || b.ChannelID != "999" || b.Worktree != filepath.Join(state, "worktrees", "feat-x") {
		t.Fatalf("%+v", b)
	}
	if _, err := os.Stat(b.Worktree); err != nil {
		t.Fatal("worktree 應已建立")
	}
	if _, err := a.Bind(context.Background(), BindOptions{Name: "feat-x", Repo: repo, ChannelID: "1"}); err == nil {
		t.Fatal("重名應拒絕")
	}
	if _, err := a.Bind(context.Background(), BindOptions{Name: "Bad Name", Repo: repo, ChannelID: "1"}); err == nil {
		t.Fatal("名稱不合法應拒絕")
	}
}

func TestBindRollsBackOnChannelFailure(t *testing.T) {
	a, d, repo, state := setup(t)
	d.fail = true
	if _, err := a.Bind(context.Background(), BindOptions{Name: "y", Repo: repo, Branch: "y", Base: "main", CreateChannel: true}); err == nil {
		t.Fatal("建頻道失敗應回錯")
	}
	if _, err := os.Stat(filepath.Join(state, "worktrees", "y")); err == nil {
		t.Fatal("失敗時應收掉剛建的 worktree")
	}
	cfg, _ := registry.Load(a.ConfigPath)
	if len(cfg.Bindings) != 0 {
		t.Fatal("失敗時不該寫進設定")
	}
}

func TestPauseSetListUnbind(t *testing.T) {
	a, d, repo, state := setup(t)
	if _, err := a.Bind(context.Background(), BindOptions{Name: "w", Repo: repo, Branch: "w", Base: "main", ChannelID: "55"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPaused("w", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Set("w", "auto_approve", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Set("w", "repo", "/x"); err == nil {
		t.Fatal("不在清單上的欄位不能改")
	}
	if _, err := a.Set("w", "activity", "maybe"); err == nil {
		t.Fatal("bool 欄位要給 true/false")
	}
	list, _ := a.List()
	if !strings.Contains(list, "`w`") || !strings.Contains(list, "暫停") || !strings.Contains(list, "自動放行") || !strings.Contains(list, "<#55>") {
		t.Fatal(list)
	}
	wt := filepath.Join(state, "worktrees", "w")
	_ = os.WriteFile(filepath.Join(wt, "draft.txt"), []byte("未提交"), 0o644)
	_ = os.MkdirAll(filepath.Join(state, "bindings", "w"), 0o755)
	msg, err := a.Unbind(context.Background(), UnbindOptions{Name: "w", DeleteChannel: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "wip commit") || !strings.Contains(msg, "封存") || len(d.deleted) != 1 {
		t.Fatalf("%s %v", msg, d.deleted)
	}
	if out, _ := exec.Command("git", "-C", repo, "show", "w:draft.txt").Output(); string(out) != "未提交" {
		t.Fatal("未提交的工作應留在分支上")
	}
	cfg, _ := registry.Load(a.ConfigPath)
	if len(cfg.Bindings) != 0 {
		t.Fatal("應已從設定移除")
	}
}
