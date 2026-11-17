package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"muninn/internal/chat"
)

type fakeFiles struct {
	mu      sync.Mutex
	fetched int
	sent    []string
}

func (f *fakeFiles) Fetch(_ context.Context, a chat.Attachment) (io.ReadCloser, error) {
	f.mu.Lock()
	f.fetched++
	f.mu.Unlock()
	if a.URL == "broken" {
		return nil, errors.New("404")
	}
	return io.NopCloser(strings.NewReader("DATA:" + a.Name)), nil
}

func (f *fakeFiles) SendFile(_ context.Context, ch, name string, data []byte, caption string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, ch+"|"+name+"|"+string(data)+"|"+caption)
	return nil
}

func TestAttachmentsDownloadedAndPrompted(t *testing.T) {
	dir := t.TempDir()
	asker, snd, files := &fakeAsker{}, &fakeSender{}, &fakeFiles{}
	a := newApp(t, dir, asker, snd, nil)
	a.Files = files
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWorker(ctx)
	go a.RunSender(ctx)
	m := msgFrom("m1", "")
	m.Attachments = []chat.Attachment{
		{Name: "../../etc/截圖 1.png", URL: "ok", ContentType: "image/png"},
		{Name: "bad.txt", URL: "broken"},
		{Name: "huge.bin", URL: "ok", Size: maxAttachmentBytes + 1},
	}
	_ = a.OnMessage(ctx, m)
	waitFor(t, func() bool { return len(snd.snapshot()) == 1 })
	asker.mu.Lock()
	prompt := asker.seen[0]
	asker.mu.Unlock()
	want := filepath.Join(AttachmentsDir(dir), jobID(m.CreatedAt, "m1"), "01_截圖 1.png")
	if !strings.Contains(prompt, want) || !strings.Contains(prompt, "3 個檔案") {
		t.Fatalf("prompt 應列出下載路徑（名稱去掉路徑跳脫）：%s", prompt)
	}
	if b, _ := os.ReadFile(want); string(b) != "DATA:../../etc/截圖 1.png" {
		t.Fatalf("檔案內容不對：%q", b)
	}
	if !strings.Contains(prompt, "bad.txt") || !strings.Contains(prompt, "下載失敗") || !strings.Contains(prompt, "超過上限") {
		t.Fatalf("失敗的附件也要說明：%s", prompt)
	}
}

func TestSendFileTool(t *testing.T) {
	dir := t.TempDir()
	files := &fakeFiles{}
	a := newApp(t, dir, &fakeAsker{}, &fakeSender{}, nil)
	a.Files, a.Workdir = files, dir
	_ = os.WriteFile(filepath.Join(dir, "out.txt"), []byte("結果"), 0o644)
	in, _ := json.Marshal(map[string]string{"path": "out.txt", "caption": "給你"})
	if r := a.HandleTool("send_file", in); r.IsError || !strings.Contains(r.Text, "out.txt") {
		t.Fatalf("%+v", r)
	}
	if len(files.sent) != 1 || files.sent[0] != "c1|out.txt|結果|給你" {
		t.Fatalf("%v", files.sent)
	}
	for _, bad := range []string{`{"path":"nope.txt"}`, `{"path":"` + dir + `"}`, `{}`} {
		if r := a.HandleTool("send_file", json.RawMessage(bad)); !r.IsError {
			t.Errorf("%s 應失敗：%+v", bad, r)
		}
	}
}
