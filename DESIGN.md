# Muninn 設計稿（草案）

> 重寫 claude_cron：拿掉 tmux，改用 headless `claude -p`。語言 Go。全新 repo。
> 名字取自奧丁的記憶渡鴉 Muninn——每天飛出去打探、回來跟主人報告，正是 agent 派到各頻道跑完回報的畫面。（拆元件時「思想」渡鴉 Huginn 可留給另一半用。）
> 狀態：M1～M5 完成，並補齊稽核出的缺口（2026-11-17，見 §15）。Discord 與 Telegram 同時運作。

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
- **M2**：registry 綁定 + 每頻道 repo/分支 worktree + 接手 claude_cron 既有 session（見 §8）。
- **M3**：permission MCP 確認鏡像 + auto-approve。
- **M4**：scheduler、多頻道、supervisor（記憶體/閒置/OOM）。
- **M5**：Telegram、活動旁白串流。

## 7. 風險（誠實列）

- **claude -p 計費**：你決定不測、算過。萬一實際走 API 不是訂閱，這是要回頭的最大風險。
- 長駐 headless 的穩定度和記憶體占用還沒實測。
- `--permission-prompt-tool` 的實際行為要到 M3 才會真的驗到。

## 8. 接手 claude_cron 既有 session（M2）

claude_cron 開的 session 就是普通 claude session（`~/.claude/projects/<目錄>/<id>.jsonl`），Muninn 用 `--resume <id>` 就能接上。2026-11-17 實測：

- resume 後 session id 不變，是接續而不是分岔。
- 從不同目錄 resume 也接得到、前文記得，worktree 搬家不影響。

做法：registry 的 binding 加 `session_id` 欄位，加一個 `muninn adopt <binding>` 指令，一次做完：

1. 從該 worktree 對應的 projects 目錄找最新的 jsonl，取得 session id（claude_cron 本身沒記 id）。
2. 停掉 claude_cron 那邊的 binding / tmux session。**同一個 session 同時只能有一個程序在寫**，兩邊同時開會把歷史寫亂。
3. 把 id 寫進 binding，Muninn 以 `--resume` 啟動。
4. 自動送一則交接說明：之後直接回文字，不用再讀 current_job.json、寫 outbox JSON。舊歷史裡全是 claude_cron 的檔案協定，不講清楚它會照舊去寫檔。

要注意：

- worktree 的 `.claude/settings.local.json` 掛著 `claude-cron permission-gate` 與 `session-hook`，headless 一樣會吃到。接手時要拿掉或改掛 M3 的權限 MCP，不然兩套權限閘同時跑。
- 長 session 接手後的第一輪要把整段歷史讀進 cache，會慢而且貴（幾十秒、幾毛錢），之後就正常了。

## 9. M1 實測紀錄（2026-11-17）

- 計費：init 事件 `apiKeySource="none"`、rate_limit_event 為 `seven_day` 訂閱視窗，確認走訂閱而非 API key。§7 第一條風險解除。
- 速度：冷啟動第一則約 33 秒，同程序第二則 4.2 秒，長駐的效益明顯。
- 記憶體：一個 binding（muninn + 一個 claude）峰值約 500MB（3 分鐘 497MB、7.5 分鐘 547MB），M4 估上限時以此為量級。
- `total_cost_usd` 是程序累計值，單則花費要自己算差額。
- 程序要自成 process group，取消時整組砍，否則子程序握著 stdout 會卡住讀取端。
- 在 Muninn 裡要 claude「把 Muninn 停掉」，它會拒絕（判斷自己跑在裡面）。停止／重啟這類指令應由 Muninn 自己攔截處理（M4 supervisor），不要交給 claude。
- `-p` 模式沒有權限 MCP 時，工作目錄外的讀取會被直接拒絕，要等 M3。

## 10. M2 實測紀錄（2026-11-17）

- 兩個測試頻道各綁一個 binding，worktree 由 Muninn 啟動時自動 `git worktree add` 建立（分支從 main 開）。
- 單一 Gateway 連線正確分派：兩頻道各記各的暗號、互不串。
- 兩個 binding 各自長駐一個 claude 程序（各自的 session id）。冷啟動第一則 7～8 秒，第二則 2.6～3.4 秒。
- 接手 claude_cron 既有 session 的實機對話尚未測試（需要先停掉一個 claude_cron binding）。

## 11. M3 實測紀錄（2026-11-17）

- `--permission-prompt-tool` 協定實測：claude 送 `{tool_name, input, tool_use_id}`；回 `{"behavior":"allow","updatedInput":<原參數>}` 才會執行，回 deny 時 claude 會照實回報、不換方法硬做。
- allow 一定要帶回原參數，給空物件 claude 會用空參數執行（單元測試抓到）。
- 頻道確認：三顆按鈕（允許一次／允許並記住／拒絕）+ 文字 y / ya / n，實機按過可用；auto_approve 直接放行。
- **使用者全域的 rtk hook 會在權限確認之前改寫指令**（`git status` → `rtk git status`），記住分類要把 rtk 當包裝跳過。
- 事故：main 漏接 Prompter，權限請求 nil panic 讓 Muninn 連續崩潰 4 次，而且每次重啟都會回放同一則訊息。已修（啟動時檢查依賴、broker 攔 panic）。

## 12. 留給 M4 的待辦

- 同一則 job 反覆讓程序崩潰時會無限回放：要加重試上限（例如 3 次），超過就放棄並通知頻道。
- 停止／重啟這類指令由 Muninn 自己攔截，不交給 claude（見 §9）。
- 記憶體：每個 binding 約 500MB，閒置回收與上限以此估算。

## 13. M4 實測紀錄（2026-11-17）

- `!status`、閒置回收（設 3 分鐘）實機生效；`!cancel` 與崩潰回放上限只有單元測試。
- scheduler 沿用 claude_cron 的 triggers.json 格式與語意；`triggers import` 拿真的 triggers.json 試過（14 個匯入、時區換算正確、原檔未動）。
- 實測抓到兩個 bug：
  - 手寫、沒有 `last_checked` 的 trigger 永遠不會觸發（每輪都從「現在」往後找）。改成從 `created_at` 起算，都沒有就第一輪寫回起算點。
  - **binding 建好、還沒收過訊息就重啟 → `--resume` 一個 claude 還沒建立的 session → "No conversation found"，這個 binding 卡死。**改成每次啟動前檢查 `~/.claude/projects/*/<id>.jsonl` 是否存在，再決定用 `--resume` 還是 `--session-id`。
- 排程訊息 ID 由排定時間導出，先寫標記（含 job id）再寫 inbox，崩潰在任一點都不會重複或漏送。

## 14. M5 實測紀錄（2026-11-17）

- 排程重寫成 Muninn 自己的功能：頻道裡的 claude 透過 Muninn MCP 工具（schedule_add / list / remove / pause / run_now）管理自己頻道的任務；不沿用、不匯入 claude_cron 的 triggers.json。
- 實測發現 `claude -p` 會載入使用者全域 skill，曾照別套系統的 skill 去建排程；改用 `--append-system-prompt` 明講只用 Muninn 工具，並停用內建 Cron 工具。整合測試只放行 mcp__muninn__*。
- 活動旁白：每輪一則「工作中」訊息原地更新（≥2 秒一次），跑完改成總結。
- Telegram：long polling、inline 按鈕、Markdown→HTML（失敗退回純文字）、/status 與 /status@bot。私訊與群組都實機驗過（對話、權限按鈕、排程建立／觸發／刪除）。
  - 群組要在 BotFather 關隱私模式**並把 bot 重新加入群組**才收得到一般訊息。
  - 有 token 就輪詢；未綁定的聊天室傳訊息時，bot 回覆 chat id 與 user id，方便設定。

## 15. 補齊缺口（2026-11-17，依 docs/gap-audit-2026-11-17.md）

| 項目 | 做法（Muninn／headless 的重做方式） |
|---|---|
| 附件 | 收：Discord 附件、Telegram 照片／文件下載到 binding 的 attachments/，路徑附在訊息後，claude 用 Read 讀；送：send_file 工具 |
| Telegram 重收 | getUpdates offset 存檔 |
| 卡住 | supervisor 看 stream-json 最後事件時間，超過 stall_timeout 且沒在等權限確認就中斷 |
| 登入失效 | 偵測 claude 的 "Not logged in · Please run /login"（實測輸出），通知一次、訊息留佇列、每 5 分鐘重試 |
| 權限 | Muninn 自己判斷（與舊版相同規則）：危險 Bash、範圍外寫入、網路與 MCP 才問 |
| 頻道管理 | 設定熱重載；CLI bind/unbind/pause/resume/set/list；控制頻道（!指令與 binding_* 工具）；自動建／刪 Discord 頻道 |
| 可靠性 | Gateway RESUME、送訊息退避重試與放棄、程式碼區塊切段、單一實例鎖、check 兼 doctor、version、make install |
| 委派 | delegate／task_* 工具：每任務獨立 worktree 與 session、角色與等級、權限轉委派者頻道、結果回委派者對話 |
| 其他 | muninn notify、muninn schedule add/rm/pause/resume/run |

| 管理後台 | 內建網頁＋JSON API（token 認證、預設只聽本機）：頻道狀態與操作、排程、委派、稽核 |
| 對外入口 | `POST /api/bindings/{name}/message`（webhook）、`POST /api/tasks`（外部委派），token 認證 |
| 稽核 | `audit.jsonl`：權限決定（含誰按的）、管理操作（cli／channel／web）、委派、API 呼叫 |
