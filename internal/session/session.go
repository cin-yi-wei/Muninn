// Package session 管理每個 binding 綁定的 claude session id，以及接手既有 session（adopt）。
package session

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Session 是持久化在 <stateDir>/session.json 的資訊。
type Session struct {
	ID          string `json:"session_id"`
	Created     string `json:"created_at"`
	AdoptedFrom string `json:"adopted_from,omitempty"` // 接手時原本的 jsonl 路徑
}

func path(stateDir string) string { return filepath.Join(stateDir, "session.json") }

// handoffPath 存在時，下一則訊息前面要加上交接說明（見 TakeHandoff）。
func handoffPath(stateDir string) string { return filepath.Join(stateDir, "handoff.txt") }

// Load 讀出既有 session；不存在時 ok=false。
func Load(stateDir string) (s Session, ok bool, err error) {
	data, err := os.ReadFile(path(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	if err := json.Unmarshal(data, &s); err != nil || s.ID == "" {
		return Session{}, false, fmt.Errorf("session: %s 內容壞掉", path(stateDir))
	}
	return s, true, nil
}

// LoadOrCreate 讀出既有 session（resume=true），沒有就產生新的 uuid 並寫檔。
func LoadOrCreate(stateDir string) (s Session, resume bool, err error) {
	s, ok, err := Load(stateDir)
	if err != nil {
		return s, false, err
	}
	if ok {
		return s, true, nil
	}
	s = Session{ID: NewUUID(), Created: now()}
	return s, false, save(stateDir, s)
}

func save(stateDir string, s Session) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return writeAtomic(path(stateDir), data)
}

func writeAtomic(p string, data []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// NewUUID 產生 v4 uuid（claude --session-id 要求合法 uuid）。
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ProjectDir 回傳 claude 存放某個工作目錄 session 的位置：
// ~/.claude/projects/<路徑中非英數字元全換成 ->。
func ProjectDir(claudeHome, workdir string) string {
	abs, err := filepath.Abs(workdir)
	if err == nil {
		workdir = abs
	}
	return filepath.Join(claudeHome, "projects", nonAlnum.ReplaceAllString(workdir, "-"))
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Candidate 是工作目錄底下找到的一個既有 session。
type Candidate struct {
	ID      string
	Path    string
	ModTime time.Time
}

// FindLatest 找出 workdir 最近修改的 session jsonl（只看主 session，不含 subagent 紀錄）。
func FindLatest(claudeHome, workdir string) (Candidate, error) {
	dir := ProjectDir(claudeHome, workdir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return Candidate{}, fmt.Errorf("session: 找不到 %s 的 session 目錄 %s: %w", workdir, dir, err)
	}
	var best Candidate
	for _, e := range ents {
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") || !uuidRE.MatchString(id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(best.ModTime) {
			best = Candidate{ID: id, Path: filepath.Join(dir, e.Name()), ModTime: info.ModTime()}
		}
	}
	if best.ID == "" {
		return Candidate{}, fmt.Errorf("session: %s 底下沒有任何 session", dir)
	}
	return best, nil
}

// DefaultHandoff 是接手舊 claude_cron session 時，第一則訊息前面附上的說明。
// 舊歷史裡全是「讀 current_job.json、寫 outbox JSON」的檔案協定，不講清楚它會照舊去寫檔。
const DefaultHandoff = `[Muninn 交接說明] 這個 session 已經從 claude_cron 交接給 Muninn。
從這則開始，使用者的訊息會直接以原文送進來，你的最終回覆文字會直接送回頻道。
不需要再讀 current_job.json、不需要寫 outbox JSON 檔，也不要呼叫任何發送訊息的指令。
以下是使用者的新訊息：

`

// Adopt 讓 binding 接手既有 session：寫入 session.json 並留下交接標記。
// 已經綁了別的 session 時要 force 才會覆蓋。
func Adopt(stateDir string, c Candidate, force bool) error {
	if cur, ok, err := Load(stateDir); err != nil {
		return err
	} else if ok && cur.ID != c.ID && !force {
		return fmt.Errorf("session: 已綁定 session %s，要改綁請加 --force", cur.ID)
	}
	if err := save(stateDir, Session{ID: c.ID, Created: now(), AdoptedFrom: c.Path}); err != nil {
		return err
	}
	return writeAtomic(handoffPath(stateDir), []byte(DefaultHandoff))
}

// TakeHandoff 回傳待附加的交接說明（沒有則空字串）。呼叫端在訊息成功送進 claude 後
// 要呼叫 ClearHandoff；送失敗就留著，下次重試再附。
func TakeHandoff(stateDir string) string {
	data, err := os.ReadFile(handoffPath(stateDir))
	if err != nil {
		return ""
	}
	return string(data)
}

// ClearHandoff 刪除交接標記。
func ClearHandoff(stateDir string) error {
	err := os.Remove(handoffPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
