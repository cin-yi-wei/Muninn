// Package worktree 確保每個 binding 的 git worktree 存在且在正確分支上。
//
// 原則：只建、不改。worktree 不存在就建；已存在但分支不對就報錯，
// 絕不自動 checkout 切分支（可能吃掉沒 commit 的工作）。
package worktree

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Ensure 讓 path 成為 repo 在 branch 上的 worktree。
// path 為空表示直接在 repo 工作，只檢查 repo 是 git 目錄（branch 有指定時也檢查分支）。
// 回傳 created=true 表示這次新建了 worktree。
func Ensure(repo, branch, base, path string) (created bool, err error) {
	if _, err := git(repo, "rev-parse", "--git-dir"); err != nil {
		return false, fmt.Errorf("worktree: %s 不是 git repo: %w", repo, err)
	}
	if path == "" {
		if branch != "" {
			return false, checkBranch(repo, branch)
		}
		return false, nil
	}
	if _, statErr := os.Stat(path); statErr == nil {
		// 已存在：必須是同一個 repo 的 worktree，而且在對的分支上
		top, err := git(path, "rev-parse", "--show-toplevel")
		if err != nil {
			return false, fmt.Errorf("worktree: %s 已存在但不是 git worktree", path)
		}
		if !samePath(top, path) {
			return false, fmt.Errorf("worktree: %s 位在另一個 git 目錄 %s 底下", path, top)
		}
		if !sameRepo(repo, path) {
			return false, fmt.Errorf("worktree: %s 不屬於 repo %s", path, repo)
		}
		return false, checkBranch(path, branch)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if _, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err = git(repo, "worktree", "add", path, branch)
		return err == nil, err
	}
	if base == "" {
		base = "HEAD"
	}
	_, err = git(repo, "worktree", "add", "-b", branch, path, base)
	return err == nil, err
}

func checkBranch(dir, branch string) error {
	cur, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if cur != branch {
		return fmt.Errorf("worktree: %s 目前在分支 %q，設定要求 %q（不會自動切換，請手動處理）", dir, cur, branch)
	}
	return nil
}

// sameRepo 比對兩個目錄的 git common dir（同一個 repo 的 worktree 共用）。
func sameRepo(a, b string) bool {
	ca, err1 := git(a, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cb, err2 := git(b, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return err1 == nil && err2 == nil && samePath(ca, cb)
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// Remove 移除 worktree：有未提交的改動時先做一個 wip commit（不丟工作），再 git worktree remove。
// 分支保留。回傳 wip commit 的 hash（沒有改動時為空）。
func Remove(repo, path string) (wip string, err error) {
	if path == "" {
		return "", fmt.Errorf("worktree: 沒有 worktree 可移除（binding 直接在 repo 工作）")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		_, _ = git(repo, "worktree", "prune")
		return "", nil // 已經不在了
	}
	if !sameRepo(repo, path) {
		return "", fmt.Errorf("worktree: %s 不屬於 repo %s，不動它", path, repo)
	}
	status, err := git(path, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := git(path, "add", "-A"); err != nil {
			return "", err
		}
		if _, err := git(path, "-c", "user.name=muninn", "-c", "user.email=muninn@localhost",
			"commit", "-q", "--no-verify", "-m", "wip: Muninn unbind 前保留未提交的工作"); err != nil {
			return "", err
		}
		if wip, err = git(path, "rev-parse", "--short", "HEAD"); err != nil {
			return "", err
		}
	}
	if _, err := git(repo, "worktree", "remove", "--force", path); err != nil {
		return wip, err
	}
	return wip, nil
}
