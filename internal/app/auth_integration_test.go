//go:build integration

package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"muninn/internal/runner"
	"muninn/internal/session"
)

// 真的 claude，指向空的設定目錄（＝沒登入），確認 isAuthFailure 認得它的輸出。
func TestIntegrationNotLoggedInDetected(t *testing.T) {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "ANTHROPIC_API_KEY=") && !strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") && !strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") {
			env = append(env, e)
		}
	}
	env = append(env, "CLAUDE_CONFIG_DIR="+t.TempDir())
	r := runner.New(runner.Config{Workdir: t.TempDir(), SessionID: session.NewUUID(), Env: env})
	defer r.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := r.Ask(ctx, "hi")
	t.Logf("rep=%+v err=%v", rep, err)
	if !isAuthFailure(rep, err) {
		t.Fatal("沒登入的 claude 輸出應被判為登入失效")
	}
}
