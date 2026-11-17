package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// 設定檔的程式化修改（CLI、聊天指令、控制頻道工具共用）。
//
// 直接改原始 JSON（map），不經過 Config 結構：保留使用者沒寫的欄位維持沒寫（不把預設值灌進去）、
// 保留 ~ 路徑寫法。改完先用 Load 驗證整份設定，通過才原子替換，並留一份 .bak。

var editMu sync.Mutex

// RawBinding 是設定檔裡一個 binding 的原始欄位。
type RawBinding = map[string]any

// Edit 讀出設定檔的 bindings 陣列交給 fn 修改，驗證通過後寫回。
func Edit(path string, fn func(bindings []RawBinding) ([]RawBinding, error)) error {
	editMu.Lock()
	defer editMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // 保留數字原樣（例如 memory_limit_mb）
	if err := dec.Decode(&root); err != nil {
		return fmt.Errorf("registry: 解析 %s: %w", path, err)
	}
	var bindings []RawBinding
	if arr, ok := root["bindings"].([]any); ok {
		for _, x := range arr {
			if m, ok := x.(map[string]any); ok {
				bindings = append(bindings, m)
			}
		}
	}
	bindings, err = fn(bindings)
	if err != nil {
		return err
	}
	if bindings == nil {
		bindings = []RawBinding{}
	}
	root["bindings"] = bindings
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	// 先寫暫存檔驗證，通過才替換
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if _, err := Load(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.WriteFile(path+".bak", data, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// FindRaw 依名稱找原始 binding 的索引，找不到回 -1。
func FindRaw(bindings []RawBinding, name string) int {
	for i, b := range bindings {
		if b["name"] == name {
			return i
		}
	}
	return -1
}
