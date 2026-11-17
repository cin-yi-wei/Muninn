// Package queue 是檔案式的訊息佇列（DESIGN 決策 B）：每則 job 一個 JSON 檔，
// 先寫 .tmp 再 rename 進 pending/，處理完搬到 done/。崩潰後 pending/ 裡留下的就是
// 還沒處理的，重啟直接回放。
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dir 是一條佇列（例如 state/inbox 或 state/outbox）。
type Dir struct {
	Root string
}

func (d Dir) pending() string { return filepath.Join(d.Root, "pending") }
func (d Dir) done() string    { return filepath.Join(d.Root, "done") }

// Init 建立 pending/ 與 done/ 目錄。
func (d Dir) Init() error {
	for _, p := range []string{d.pending(), d.done()} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Put 以 id 為檔名原子寫入一則 job。id 已存在（pending 或 done）時回傳 ErrExists，
// 讓同一則 Discord 訊息重送時不會被處理兩次。
func (d Dir) Put(id string, v any) error {
	if strings.ContainsAny(id, `/\`) || id == "" {
		return fmt.Errorf("queue: 不合法的 id %q", id)
	}
	name := id + ".json"
	for _, p := range []string{filepath.Join(d.pending(), name), filepath.Join(d.done(), name)} {
		if _, err := os.Stat(p); err == nil {
			return ErrExists
		}
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(d.pending(), "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d.pending(), name))
}

// ErrExists 表示同 id 的 job 已經在佇列裡（或處理過了）。
var ErrExists = errors.New("queue: job 已存在")

// Pending 依檔名排序列出待處理 job 的 id（id 以時間開頭時即為先進先出）。
func (d Dir) Pending() ([]string, error) {
	ents, err := os.ReadDir(d.pending())
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(n, ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

// Get 讀出一則待處理 job。
func (d Dir) Get(id string, v any) error {
	data, err := os.ReadFile(filepath.Join(d.pending(), id+".json"))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Done 把 job 從 pending/ 搬到 done/。
func (d Dir) Done(id string) error {
	return os.Rename(filepath.Join(d.pending(), id+".json"), filepath.Join(d.done(), id+".json"))
}
