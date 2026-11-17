package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"muninn/internal/permission"
)

// pendingPerm 是一則貼在頻道上、等使用者回 y/n 的權限請求。
type pendingPerm struct {
	n  int
	ch chan permission.Decision // 容量 1，回覆者寫入一次
}

// permState 是每個 App 的權限等待佇列（claude 可能同時發出多個工具呼叫）。
type permState struct {
	mu      sync.Mutex
	queue   []*pendingPerm
	counter int
}

var (
	yesWords = map[string]bool{"y": true, "yes": true, "ok": true, "允許": true, "好": true, "可以": true, "同意": true}
	noWords  = map[string]bool{"n": true, "no": true, "拒絕": true, "不要": true, "不行": true, "不可以": true}
)

// parseYesNo 判斷訊息是不是 y/n 回覆。
func parseYesNo(s string) (allow, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, "。.!！")
	switch {
	case yesWords[s]:
		return true, true
	case noWords[s]:
		return false, true
	}
	return false, false
}

// RequestPermission 是給 Broker 的 Handler：auto_approve 直接放行，否則貼到頻道等 y/n。
func (a *App) RequestPermission(ctx context.Context, req permission.Request) permission.Decision {
	desc := permission.Describe(req.ToolName, req.Input)
	if a.AutoApprove {
		a.logf("permission: auto_approve 放行 %s", req.ToolName)
		return permission.Allow(req.Input)
	}

	a.perm.mu.Lock()
	a.perm.counter++
	p := &pendingPerm{n: a.perm.counter, ch: make(chan permission.Decision, 1)}
	a.perm.queue = append(a.perm.queue, p)
	a.perm.mu.Unlock()

	timeout := a.PermissionTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	msg := fmt.Sprintf("🔐 **權限確認 #%d**　claude 要用 %s\n回 `y` 允許、`n` 拒絕（%s 內沒回自動拒絕）", p.n, desc, timeout)
	if err := a.Sender.Send(ctx, a.ChannelID, msg, ""); err != nil {
		a.removePending(p)
		a.logf("permission: 貼確認訊息失敗: %v", err)
		return permission.Deny("Muninn 無法把確認貼到頻道，已拒絕")
	}
	a.logf("permission: #%d 等待確認 %s", p.n, req.ToolName)

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case d := <-p.ch:
		if d.Behavior == "allow" {
			d.UpdatedInput = req.Input // 一定帶回原參數，否則 claude 會用空參數執行
		}
		return d
	case <-t.C:
		a.removePending(p)
		a.notify(fmt.Sprintf("⌛ 權限 #%d 逾時，已拒絕", p.n))
		return permission.Deny(fmt.Sprintf("使用者 %s 內沒有回覆，已拒絕", timeout))
	case <-ctx.Done():
		// claude 被砍或 Muninn 關機
		a.removePending(p)
		return permission.Deny("請求已取消")
	}
}

func (a *App) removePending(p *pendingPerm) {
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	for i, q := range a.perm.queue {
		if q == p {
			a.perm.queue = append(a.perm.queue[:i], a.perm.queue[i+1:]...)
			return
		}
	}
}

// notify 送一則系統短訊到頻道，失敗只記 log。
func (a *App) notify(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.Sender.Send(ctx, a.ChannelID, text, ""); err != nil {
		a.logf("notify 失敗: %v", err)
	}
}

// handlePermReply 在 OnMessage 裡先攔截：有待確認的請求且訊息是 y/n，就拿去回最早的那一則。
// 回傳 true 表示訊息已被消化，不進 inbox。
func (a *App) handlePermReply(content string) bool {
	a.perm.mu.Lock()
	if len(a.perm.queue) == 0 {
		a.perm.mu.Unlock()
		return false
	}
	allow, ok := parseYesNo(content)
	if !ok {
		n := a.perm.queue[0].n
		a.perm.mu.Unlock()
		// 不是 y/n：照常排隊，但提醒還有確認在等（claude 卡在等確認，這則要等它跑完才會處理）
		go a.notify(fmt.Sprintf("（還在等權限 #%d 的 y/n，這則訊息先排隊）", n))
		return false
	}
	p := a.perm.queue[0]
	a.perm.queue = a.perm.queue[1:]
	a.perm.mu.Unlock()

	if allow {
		p.ch <- permission.Allow(nil) // updatedInput 由 RequestPermission 補回原參數
		go a.notify(fmt.Sprintf("✅ 已允許 #%d", p.n))
	} else {
		p.ch <- permission.Deny("使用者拒絕了這個操作")
		go a.notify(fmt.Sprintf("🚫 已拒絕 #%d", p.n))
	}
	return true
}
