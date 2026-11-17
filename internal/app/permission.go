package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"muninn/internal/discord"
	"muninn/internal/permission"
)

// Prompter 是權限確認需要的 Discord 能力（帶按鈕的訊息、改寫訊息）。
type Prompter interface {
	SendPrompt(ctx context.Context, channelID, text string, buttons []discord.Button) (string, error)
	EditMessage(ctx context.Context, channelID, messageID, text string) error
}

// 按鈕動作。
const (
	actAllow    = "allow"
	actRemember = "remember"
	actDeny     = "deny"
)

// customIDPrefix 標記 Muninn 的權限按鈕；custom_id 為 muninn|<動作>|<請求 id>。
// 請求 id 是隨機值，Muninn 重啟後舊按鈕自然對不上，不會誤批到新的請求。
const customIDPrefix = "muninn"

// pendingPerm 是一則貼在頻道上、等使用者決定的權限請求。
type pendingPerm struct {
	id    string
	n     int
	desc  string
	keys  []string // 「允許並記住」要記的分類；nil 表示不能記住
	msgID string
	ch    chan permission.Decision // 容量 1，決定者寫入一次
}

// permState 是每個 App 的權限等待佇列（claude 可能同時發出多個工具呼叫）。
type permState struct {
	mu      sync.Mutex
	queue   []*pendingPerm
	counter int
	store   *permission.Store
}

var (
	rememberWords = map[string]bool{"ya": true, "yy": true, "always": true, "記住": true, "都允許": true, "永遠": true}
	yesWords      = map[string]bool{"y": true, "yes": true, "ok": true, "允許": true, "好": true, "可以": true, "同意": true}
	noWords       = map[string]bool{"n": true, "no": true, "拒絕": true, "不要": true, "不行": true, "不可以": true}
)

// parseDecision 判斷訊息是不是確認回覆，回傳動作（空字串＝不是）。
func parseDecision(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, "。.!！")
	switch {
	case rememberWords[s]:
		return actRemember
	case yesWords[s]:
		return actAllow
	case noWords[s]:
		return actDeny
	}
	return ""
}

func randomID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (a *App) store() *permission.Store {
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	if a.perm.store == nil {
		a.perm.store = permission.NewStore(a.StateDir)
	}
	return a.perm.store
}

// RequestPermission 是給 Broker 的 Handler：auto_approve 或記住過的分類直接放行，否則貼到頻道等決定。
func (a *App) RequestPermission(ctx context.Context, req permission.Request) permission.Decision {
	if a.AutoApprove {
		a.logf("permission: auto_approve 放行 %s", req.ToolName)
		return permission.Allow(req.Input)
	}
	keys := permission.Keys(req.ToolName, req.Input)
	if a.store().Covers(keys) {
		a.logf("permission: 已記住 %v，放行", keys)
		return permission.Allow(req.Input)
	}

	a.perm.mu.Lock()
	a.perm.counter++
	p := &pendingPerm{id: randomID(), n: a.perm.counter, keys: keys, ch: make(chan permission.Decision, 1)}
	p.desc = fmt.Sprintf("🔐 **權限確認 #%d**　claude 要用 %s", p.n, permission.Describe(req.ToolName, req.Input))
	a.perm.queue = append(a.perm.queue, p)
	a.perm.mu.Unlock()

	timeout := a.PermissionTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	hint := "也可以直接回 `y` / `ya`（記住）/ `n`"
	if keys == nil {
		hint = "這個指令無法拆解，不能記住。也可以直接回 `y` / `n`"
	} else {
		hint += "\n記住的範圍：" + formatKeys(keys)
	}
	text := fmt.Sprintf("%s\n%s（%s 內沒回自動拒絕）", p.desc, hint, timeout)
	buttons := []discord.Button{
		{Label: "允許一次", CustomID: customIDPrefix + "|" + actAllow + "|" + p.id, Style: 3},
		{Label: "允許並記住", CustomID: customIDPrefix + "|" + actRemember + "|" + p.id, Style: 1},
		{Label: "拒絕", CustomID: customIDPrefix + "|" + actDeny + "|" + p.id, Style: 4},
	}
	if keys == nil {
		buttons = append(buttons[:1], buttons[2])
	}
	msgID, err := a.Prompter.SendPrompt(ctx, a.ChannelID, text, buttons)
	if err != nil {
		a.removePending(p)
		a.logf("permission: 貼確認訊息失敗: %v", err)
		return permission.Deny("Muninn 無法把確認貼到頻道，已拒絕")
	}
	a.perm.mu.Lock()
	p.msgID = msgID
	a.perm.mu.Unlock()
	a.logf("permission: #%d 等待確認 %s keys=%v", p.n, req.ToolName, keys)

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case d := <-p.ch:
		if d.Behavior == "allow" {
			d.UpdatedInput = req.Input // 一定帶回原參數，否則 claude 會用空參數執行
		}
		return d
	case <-t.C:
		if a.removePending(p) {
			a.edit(p, "⌛ 逾時，已自動拒絕")
			a.notify(fmt.Sprintf("⌛ 權限 #%d 逾時，已拒絕", p.n))
		}
		return permission.Deny(fmt.Sprintf("使用者 %s 內沒有回覆，已拒絕", timeout))
	case <-ctx.Done():
		// claude 被砍或 Muninn 關機
		if a.removePending(p) {
			a.edit(p, "⚪ 已取消（claude 中止了這個請求）")
		}
		return permission.Deny("請求已取消")
	}
}

func formatKeys(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = "`" + k + "`"
	}
	return strings.Join(q, "、")
}

// removePending 把 p 移出佇列；回傳 false 表示已經被別人處理掉了。
func (a *App) removePending(p *pendingPerm) bool {
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	for i, q := range a.perm.queue {
		if q == p {
			a.perm.queue = append(a.perm.queue[:i], a.perm.queue[i+1:]...)
			return true
		}
	}
	return false
}

// takePending 依 id 取出等待中的請求（id 為空取最早的一則）。
func (a *App) takePending(id string) *pendingPerm {
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	for i, q := range a.perm.queue {
		if id == "" || q.id == id {
			a.perm.queue = append(a.perm.queue[:i], a.perm.queue[i+1:]...)
			return q
		}
	}
	return nil
}

// decide 套用決定，回傳結果那一行。
func (a *App) decide(p *pendingPerm, action string) string {
	var line string
	switch action {
	case actRemember:
		if p.keys == nil {
			line = "✅ 已允許（這個指令不能記住，只允許這一次）"
		} else if err := a.store().Add(p.keys); err != nil {
			a.logf("permission: 記住失敗: %v", err)
			line = "✅ 已允許（但記住失敗：" + err.Error() + "）"
		} else {
			line = "🧠 已允許並記住：" + formatKeys(p.keys)
		}
		p.ch <- permission.Allow(nil)
	case actAllow:
		line = "✅ 已允許一次"
		p.ch <- permission.Allow(nil) // updatedInput 由 RequestPermission 補回原參數
	default:
		line = "🚫 已拒絕"
		p.ch <- permission.Deny("使用者拒絕了這個操作")
	}
	a.logf("permission: #%d %s", p.n, line)
	return line
}

// edit 把確認訊息改寫成結果並拿掉按鈕，失敗只記 log。
func (a *App) edit(p *pendingPerm, line string) {
	if p.msgID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.Prompter.EditMessage(ctx, a.ChannelID, p.msgID, p.desc+"\n"+line); err != nil {
		a.logf("permission: 改寫確認訊息失敗: %v", err)
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

// ButtonResult 是按鈕點擊的處理結果，交給呼叫端回應 Discord 互動。
type ButtonResult struct {
	Handled bool // false：不是 Muninn 的權限按鈕，忽略
	Update  bool // true：用 Text 改寫按鈕所在的訊息；false：只回給按的人看（ephemeral）
	Text    string
}

// HandleButton 處理權限按鈕點擊。只有白名單裡的人能按。
func (a *App) HandleButton(customID, userID string) ButtonResult {
	parts := strings.SplitN(customID, "|", 3)
	if len(parts) != 3 || parts[0] != customIDPrefix {
		return ButtonResult{}
	}
	action, id := parts[1], parts[2]
	if action != actAllow && action != actRemember && action != actDeny {
		return ButtonResult{}
	}
	if len(a.AllowedUsers) > 0 && !a.AllowedUsers[userID] {
		a.logf("permission: 非白名單使用者 %s 按了按鈕，忽略", userID)
		return ButtonResult{Handled: true, Text: "你不在這個頻道的白名單裡，不能核准。"}
	}
	p := a.takePending(id)
	if p == nil {
		// 已逾時、已用文字回過、或 Muninn 重啟過
		return ButtonResult{Handled: true, Update: true, Text: "⚪ 這則確認已經失效（逾時、已處理過，或 Muninn 重啟過）"}
	}
	// 保留原本的請求內容，讓人事後查得到批了什麼
	return ButtonResult{Handled: true, Update: true, Text: p.desc + "\n" + a.decide(p, action)}
}

// handlePermReply 在 OnMessage 裡先攔截：有待確認的請求且訊息是確認回覆，就拿去回最早的那一則。
// 回傳 true 表示訊息已被消化，不進 inbox。
func (a *App) handlePermReply(content string) bool {
	a.perm.mu.Lock()
	if len(a.perm.queue) == 0 {
		a.perm.mu.Unlock()
		return false
	}
	n := a.perm.queue[0].n
	a.perm.mu.Unlock()

	action := parseDecision(content)
	if action == "" {
		// 不是確認回覆：照常排隊，但提醒還有確認在等（claude 卡在等確認，這則要等它跑完才會處理）
		go a.notify(fmt.Sprintf("（還在等權限 #%d 的決定，這則訊息先排隊）", n))
		return false
	}
	p := a.takePending("")
	if p == nil {
		return false // 剛好同時逾時了，當一般訊息
	}
	line := a.decide(p, action)
	go func() {
		a.edit(p, line)
		a.notify(fmt.Sprintf("%s（#%d）", line, p.n)) // 確認訊息可能已被洗上去，補一則短訊
	}()
	return true
}
