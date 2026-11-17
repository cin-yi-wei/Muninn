package app

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"muninn/internal/runner"
)

// 健康監控（補上舊系統的「卡住 watchdog」與「登入失效偵測」，改用 headless 的訊號）：
//
//   - 卡住：claude 正在跑一輪，卻超過 StallTimeout 沒有任何 stream-json 事件，
//     而且沒有在等使用者確認權限 → 判定卡住，中斷這一輪（session 保留）。
//   - 登入失效：result 回 "Not logged in · Please run /login" 這類錯誤（實測 claude 2.1.326 的輸出），
//     或程序提前結束且 stderr 帶同樣字樣 → 通知頻道一次、暫停處理，訊息留在佇列，每 authRetry 重試一次。

var errAuthBlocked = errors.New("claude 登入失效，等待恢復")

const authRetry = 5 * time.Minute

var authMarkers = []string{
	"please run /login", "not logged in", "oauth token has expired", "invalid api key",
	"authentication_error", "401 unauthorized", "token has been revoked",
}

// isAuthFailure 判斷這一輪是不是因為登入失效而失敗。
func isAuthFailure(rep runner.Reply, err error) bool {
	var s string
	switch {
	case err != nil && !errors.Is(err, runner.ErrInterrupted):
		s = err.Error()
	case rep.IsError:
		s = rep.Text
	default:
		return false
	}
	s = strings.ToLower(s)
	for _, m := range authMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

type authState struct {
	mu    sync.Mutex
	down  bool
	until time.Time
}

func (a *App) authBlocked() bool {
	a.auth.mu.Lock()
	defer a.auth.mu.Unlock()
	return a.auth.down && time.Now().Before(a.auth.until)
}

func (a *App) markAuthDown(rep runner.Reply, err error) {
	a.auth.mu.Lock()
	first := !a.auth.down
	a.auth.down = true
	a.auth.until = time.Now().Add(authRetry)
	a.auth.mu.Unlock()
	detail := rep.Text
	if err != nil {
		detail = err.Error()
	}
	a.logf("health: claude 登入失效（%s），%s 後重試", firstLine(detail), authRetry)
	if first {
		go a.notify(fmt.Sprintf("🔑 **claude 登入失效**：%s\n請到跑 Muninn 的機器上執行 `claude`，輸入 `/login` 重新登入。\n排隊中的訊息不會丟，Muninn 每 %s 自動重試一次，登入恢復後會接著處理。",
			firstLine(detail), authRetry))
	}
}

func (a *App) markAuthUp() {
	a.auth.mu.Lock()
	was := a.auth.down
	a.auth.down = false
	a.auth.mu.Unlock()
	if was {
		a.logf("health: claude 登入恢復")
		go a.notify("✅ claude 登入恢復，繼續處理排隊的訊息")
	}
}

// checkStall 由 supervisor 定期呼叫：卡住就中斷這一輪。回傳是否中斷了。
func (a *App) checkStall(st runner.Status, limit time.Duration) bool {
	if limit <= 0 || !st.Busy || st.Silent < limit {
		return false
	}
	a.perm.mu.Lock()
	waiting := len(a.perm.queue) > 0
	a.perm.mu.Unlock()
	if waiting {
		return false // 在等使用者按確認，安靜是正常的
	}
	if !a.Ctl.Interrupt() {
		return false
	}
	a.logf("health: claude %s 沒有任何動靜，判定卡住，已中斷", st.Silent.Round(time.Second))
	go a.notify(fmt.Sprintf("⚠️ claude 已經 %s 沒有任何動靜，判定卡住，已中斷這一輪（session 保留，可以接著講）。", st.Silent.Round(time.Minute)))
	return true
}
