package tasks

import (
	"fmt"
	"sort"
	"strings"
)

// Role 是委派任務的角色：決定附加給子 claude 的指示。
type Role struct {
	Name        string
	Description string
	Prompt      string
}

// 共通指示：每個任務都會帶。
const commonPrompt = `你是 Muninn 派出去的子任務 agent，在一個專屬的 git worktree 裡獨立工作，沒有人會即時跟你對話。
- 委派你的人（另一個 claude）只看得到你「最後一則回覆」，所以最後一則要是完整的報告：做了什麼、結論、證據（檔案:行號、指令輸出）、還沒做完或不確定的地方。
- 不要問問題等回覆；資訊不足就用最合理的假設往下做，並在報告裡寫明假設。
- 需要確認的危險操作會轉到委派者的頻道讓人核准，被拒絕就換方法或在報告裡說明。
- 不要用任何會對外發訊息的工具；不要修改 worktree 以外的檔案。`

var builtinRoles = map[string]Role{
	"general": {
		Name: "general", Description: "通用：照指示把事情做完",
		Prompt: "照委派的指示把事情做完。",
	},
	"researcher": {
		Name: "researcher", Description: "查證與調查：把問題查清楚並附出處，不改檔",
		Prompt: "你的工作是把問題查清楚：讀程式碼、查文件、跑唯讀指令驗證。每個結論都附出處（檔案:行號、指令與輸出）。不要修改任何檔案。",
	},
	"reviewer": {
		Name: "reviewer", Description: "審查：找出改動會在什麼情況下出錯，只報問題不修",
		Prompt: "你的工作是審查程式碼或改動，找出會出錯的情況：邏輯錯誤、邊界條件、併發、錯誤處理、安全問題。每個問題附檔案:行號、觸發條件與嚴重程度。只報告，不要修改檔案。",
	},
	"implementer": {
		Name: "implementer", Description: "實作：照規格寫出來，先寫失敗的測試再實作，完成後 commit",
		Prompt: "你的工作是照規格把東西實作出來：先寫會失敗的測試，再實作到測試通過，跑完整測試確認沒有弄壞別的。不要擴張範圍。完成後在這個 worktree 的分支上 commit（訊息寫清楚做了什麼），報告裡附 commit hash 與測試結果。",
	},
	"tester": {
		Name: "tester", Description: "補測試：為既有行為補上會抓到問題的測試，只動測試檔",
		Prompt: "你的工作是為既有行為補上有意義的測試（實作壞掉時會失敗的那種）。只修改或新增測試檔；發現 bug 只在報告裡說明，不要順手修。完成後 commit，報告裡附 commit hash 與測試結果。",
	},
}

// LookupRole 依名稱找角色；空字串回傳 general。
func LookupRole(name string) (Role, error) {
	if name == "" {
		name = "general"
	}
	r, ok := builtinRoles[strings.ToLower(name)]
	if !ok {
		return Role{}, fmt.Errorf("沒有角色 %q（可用：%s）", name, strings.Join(RoleNames(), "、"))
	}
	return r, nil
}

// RoleNames 回傳所有角色名稱（排序過）。
func RoleNames() []string {
	var names []string
	for n := range builtinRoles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RoleSummary 回傳角色清單說明（給工具描述用）。
func RoleSummary() string {
	var lines []string
	for _, n := range RoleNames() {
		lines = append(lines, n+"："+builtinRoles[n].Description)
	}
	return strings.Join(lines, "；")
}

// systemPrompt 組出子 claude 的附加 system prompt。
func systemPrompt(r Role, level string) string {
	lv := "你有 develop 權限：可以在 worktree 裡改檔與 commit。"
	if level == LevelReadonly {
		lv = "你只有 readonly 權限：不能修改任何檔案（寫檔工具已停用），只能讀與跑唯讀指令。"
	}
	return commonPrompt + "\n\n角色：" + r.Name + "\n" + r.Prompt + "\n" + lv
}
