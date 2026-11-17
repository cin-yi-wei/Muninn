# Muninn 設計稿（草案）

> 重寫 claude_cron：拿掉 tmux，改用 headless `claude -p`。語言 Go。全新 repo。
> 名字取自奧丁的記憶渡鴉 Muninn——每天飛出去打探、回來跟主人報告，正是 agent 派到各頻道跑完回報的畫面。（拆元件時「思想」渡鴉 Huginn 可留給另一半用。）
> 狀態：設計定稿，進入 M1 實作。拍板：長駐 session + 閒置回收、訊息佇列留檔案、里程碑 M1→M5 照走。

## 1. 為什麼重寫

現行 claude_cron 用 tmux 跑互動式 claude，控制端靠 `send-keys` 注入訊息、`capture-pane` 刮畫面讀輸出。這條路製造一整類 bug——注入搶字、bracketed paste 卡死、C-c 打斷還在開機的 session、排隊訊息被誤判成殘留、inject 死鎖握住不放——而且慢。

claude CLI 現在 headless（`-p`）功能已經完整，可以用結構化事件取代刮畫面。重寫的唯一核心目標：**把 tmux 那層整個換成 headless runner**。

## 2. 技術地基（已驗，claude 2.1.326）

這些 flag 都實際存在、不是空想：

- `claude -p --output-format stream-json --input-format stream-json`：結構化事件進出，不再刮畫面。
- `--session-id <uuid>` / `--resume <id>` / `--continue`：每個頻道綁一個固定 session、續接對話。
- `--permission-prompt-tool mcp__...`：把權限請求導到我們自己的 MCP 工具 → 貼到頻道等 y/n → 回 allow/deny。**這就是「危險指令確認鏡像到頻道」的乾淨解法**，不用再刮畫面猜有沒有跳框。
- `--permission-mode bypassPermissions` / `--dangerously-skip-permissions`：自動放行的旁路。
- `--mcp-config` / `--strict-mcp-config`：掛我們自己的 permission / notify MCP。

## 3. 架構元件

1. **ingest（gateway）**：Discord Gateway websocket 收 MESSAGE_CREATE（之後加 Telegram）→ 產生一則 inbox job。
2. **registry**：bindings 設定，頻道 ↔ repo + 分支 + worktree + 旗標（auto_approve / paused …）。概念沿用現有 bindings.json。
3. **runner（核心）**：每個 binding 一個 headless claude session。讀 stream-json 事件：assistant 文字 → 回頻道；tool_use → 活動旁白；result → 收尾。session 用 `--session-id` 綁定、`--resume` 續接。
4. **permission MCP**：實作 `--permission-prompt-tool`，把危險操作貼頻道、等你回覆、回決策；該 binding 開了 auto_approve 就直接 allow。
5. **outbox / sender**：runner 產出的回覆送回頻道。
6. **scheduler**：cron 定時注入訊息（沿用現有 trigger 語意）。
7. **supervisor**：啟停、閒置回收、記憶體上限、OOM 自保、重啟。

## 4. 三個要你拍板的決策

- **A. 長駐 session vs 每則訊息一個程序。** 長駐＝快、省掉每次 OAuth 握手，但要管生命週期和記憶體；每則訊息冷啟＝乾淨無狀態但慢。建議：長駐 + 閒置回收。
- **B. 訊息佇列用檔案還是純內存。** 檔案 inbox/outbox 好 debug、崩潰能回放（現在就是這樣）；純內存簡單但崩了就掉。建議：留檔案佇列。
- **C. 權限呈現。** 走上面說的 permission-prompt-tool MCP。這個應該沒爭議，但列出來讓你知道確認是怎麼跑的。

## 5. 跟舊版的差異

- **丟掉**：tmux、send-keys、capture-pane、整包 inject、靠刮畫面的 login-detector。
- **保留概念（重做實作）**：registry binding、inbox/outbox、scheduler、auto-approve、確認鏡像到頻道（改用 MCP）。

## 6. 里程碑（就算全做，也要分段才驗得了）

- **M1**：單一 Discord 頻道 → headless runner → 回話。證明 stream-json 跑得通、計費扣對邊。
- **M2**：registry 綁定 + 每頻道 repo/分支 worktree。
- **M3**：permission MCP 確認鏡像 + auto-approve。
- **M4**：scheduler、多頻道、supervisor（記憶體/閒置/OOM）。
- **M5**：Telegram、活動旁白串流。

## 7. 風險（誠實列）

- **claude -p 計費**：你決定不測、算過。萬一實際走 API 不是訂閱，這是要回頭的最大風險。
- 長駐 headless 的穩定度和記憶體占用還沒實測。
- `--permission-prompt-tool` 的實際行為要到 M3 才會真的驗到。
