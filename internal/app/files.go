package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"muninn/internal/chat"
	"muninn/internal/permission"
)

// Files 是平台的檔案能力：下載使用者傳來的附件、上傳檔案到頻道。
type Files interface {
	Fetch(ctx context.Context, a chat.Attachment) (io.ReadCloser, error)
	SendFile(ctx context.Context, channelID, name string, data []byte, caption string) error
}

const (
	maxAttachmentBytes = 25 << 20           // 單一附件下載上限
	attachmentKeep     = 7 * 24 * time.Hour // 附件保留多久
)

// AttachmentsDir 是這個 binding 存附件的地方（claude 要用 --add-dir 加進可讀範圍）。
func AttachmentsDir(stateDir string) string { return filepath.Join(stateDir, "attachments") }

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}._ -]`)

func safeName(name string, i int) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafeName.ReplaceAllString(name, "_")
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return fmt.Sprintf("%02d_%s", i+1, name)
}

// downloadAttachments 把附件下載到 attachments/<job id>/，回傳要附加在訊息後面的說明。
// 已經下載過（例如崩潰後重跑）就直接沿用。
func (a *App) downloadAttachments(ctx context.Context, jobID string, atts []chat.Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	dir := filepath.Join(AttachmentsDir(a.StateDir), jobID)
	var lines []string
	for i, att := range atts {
		path := filepath.Join(dir, safeName(att.Name, i))
		desc := att.Name
		if att.ContentType != "" {
			desc += "，" + att.ContentType
		}
		if err := a.fetchTo(ctx, att, path); err != nil {
			a.logf("attachments: %s 下載失敗: %v", att.Name, err)
			lines = append(lines, fmt.Sprintf("- %s（%s）：下載失敗：%v", att.Name, desc, err))
			continue
		}
		info, _ := os.Stat(path)
		lines = append(lines, fmt.Sprintf("- %s（%s，%s）", path, desc, humanSize(info.Size())))
	}
	return "\n\n[Muninn] 使用者附了 " + fmt.Sprint(len(atts)) + " 個檔案，已下載到本機，請用 Read 工具讀取（圖片也能直接看）：\n" + strings.Join(lines, "\n")
}

func (a *App) fetchTo(ctx context.Context, att chat.Attachment, path string) error {
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return nil
	}
	if a.Files == nil {
		return fmt.Errorf("這個平台不支援附件")
	}
	if att.Size > maxAttachmentBytes {
		return fmt.Errorf("檔案 %s 超過上限 %s", humanSize(att.Size), humanSize(maxAttachmentBytes))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	rc, err := a.Files.Fetch(dctx, att)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(rc, maxAttachmentBytes+1))
	f.Close()
	if err == nil && n > maxAttachmentBytes {
		err = fmt.Errorf("檔案超過上限 %s", humanSize(maxAttachmentBytes))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// pruneAttachments 刪掉超過保留期限的附件目錄。
func (a *App) pruneAttachments() {
	dir := AttachmentsDir(a.StateDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		info, err := e.Info()
		if err == nil && e.IsDir() && time.Since(info.ModTime()) > attachmentKeep {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// sendFileTool 處理 claude 的 send_file 工具：把本機檔案傳到這個頻道。
func (a *App) sendFileTool(input json.RawMessage) permission.ToolResult {
	var in struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.Path == "" {
		return toolErr("要給 path（檔案的絕對路徑）")
	}
	if a.Files == nil {
		return toolErr("這個平台不支援傳檔案")
	}
	path := in.Path
	if !filepath.IsAbs(path) && a.Workdir != "" {
		path = filepath.Join(a.Workdir, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return toolErr("讀不到檔案：%v", err)
	}
	if info.IsDir() {
		return toolErr("%s 是目錄，請先打包成單一檔案", path)
	}
	if info.Size() > 50<<20 {
		return toolErr("檔案 %s 太大", humanSize(info.Size()))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return toolErr("讀檔失敗：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := a.Files.SendFile(ctx, a.ChannelID, filepath.Base(path), data, in.Caption); err != nil {
		return toolErr("傳送失敗：%v", err)
	}
	a.logf("files: 已傳送 %s（%s）", path, humanSize(info.Size()))
	return permission.ToolResult{Text: fmt.Sprintf("已傳送 %s（%s）到頻道", filepath.Base(path), humanSize(info.Size()))}
}
