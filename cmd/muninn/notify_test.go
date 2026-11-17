package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotifyAcceptsNameOrChannelID(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "muninn.json")
	body := fmt.Sprintf(`{"state_dir":%q,"allowed_users":["u"],"bindings":[{"name":"daily","channel_id":"100000000000000001","repo":%q}]}`, state, repo)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdNotify(cfgPath, []string{"daily", "用名稱"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdNotify(cfgPath, []string{"100000000000000001", "用頻道", "id"}); err != nil {
		t.Fatalf("舊腳本傳頻道 id 也要能用：%v", err)
	}
	if err := cmdNotify(cfgPath, []string{"999", "x"}); err == nil || !strings.Contains(err.Error(), "對不上") {
		t.Fatalf("對不上的要報錯：%v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(state, "bindings", "daily", "outbox", "pending"))
	var texts []string
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(state, "bindings", "daily", "outbox", "pending", e.Name()))
		texts = append(texts, string(b))
	}
	all := strings.Join(texts, "\n")
	if len(ents) != 2 || !strings.Contains(all, "用名稱") || !strings.Contains(all, "用頻道 id") {
		t.Fatalf("outbox 應有兩則：%q", all)
	}
}
