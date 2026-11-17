# Muninn

claude_cron 的重寫版：拿掉 tmux，改用 headless `claude -p`（stream-json 進出），
讓每個聊天頻道對應一個常駐的 Claude session，訊息進來跑完回頻道。

名字取自奧丁的記憶渡鴉 Muninn——飛出去打探、回來報告。

設計與決策見 [DESIGN.md](DESIGN.md)。狀態：M1～M5 完成，Discord 與 Telegram 都可用。

## 使用方式

設定檔：複製 `muninn.example.json` 成 `muninn.json` 再改（`muninn.json` 不進 git）。
bot token 不寫在設定檔，從 `token_env` 指定的環境變數讀（預設 `DISCORD_BOT_TOKEN`）。

```sh
go build -o bin/muninn ./cmd/muninn

./bin/muninn -config muninn.json check              # 驗設定、建好缺的 worktree，不連 Discord
./bin/muninn -config muninn.json adopt <binding>    # 接手該 worktree 最近的既有 claude session
DISCORD_BOT_TOKEN=... ./bin/muninn -config muninn.json run
```

- 每個 binding 一個頻道、一個 worktree、一個長駐 claude session；整個 bot 共用一條 Gateway 連線。
- worktree 不存在會自動 `git worktree add`（分支不存在就從 `base` 開）。已存在但分支不對時會報錯，**不會自動切分支**。
- 狀態放在 `state_dir/bindings/<name>/`：inbox/outbox 佇列、`session.json`。崩潰重啟會回放 pending 裡的 job，並用 `--resume` 接回 session。
- `adopt` 之後，下一則訊息前面會自動附上交接說明（告訴 claude 之後直接回文字）。
  **同一個 session 同時只能有一個程序在寫**：原本在用它的程序一定要先停掉再 `run`。

權限確認（M3）。哪些要問由 Muninn 決定（規則與舊版相同）：

- **Bash**：只有危險指令才問（安裝套件、下載、sudo、rm -rf、mkfs、dd 等），其他直接放行
- **Edit / Write**：寫在工作目錄、binding 狀態目錄、session scratchpad、專案記憶目錄裡直接放行，其他地方要問
- **WebFetch / WebSearch / MCP 工具**：要問（`schedule_list` 除外）
- **Read、Grep 等唯讀工具**：放行

要問的時候 Muninn 會把它貼到頻道：

```
🔐 權限確認 #1　claude 要用 `Bash`：…
回 y 允許、n 拒絕（30m 內沒回自動拒絕）
```

- 確認訊息底下有三顆按鈕：**允許一次**、**允許並記住**、**拒絕**。也可以直接回文字：`y` 允許、`ya` 允許並記住、`n` 拒絕（文字回覆對應最早那則）。
- 「記住」以分類為單位存在 `bindings/<name>/permissions.json`：Bash 取指令名（git、go 這類再加子指令，如 `bash:git push`），其他工具取工具名。串接的指令（`&&`、`|`、`;`）要每段都記住過才自動放行；含 `$( )` 或反引號的不能記住。只有白名單裡的人能按按鈕。
- 等確認期間傳的其他訊息照常排隊（claude 正卡在等確認，要等這輪跑完才會處理）。
- 逾時、貼不出確認、Muninn 權限服務連不上，一律當**拒絕**。
- binding 設 `"auto_approve": true` 就直接放行、不貼頻道。逾時長度由 `permission_timeout` 設定。
- 實作：claude 的 `--permission-prompt-tool` 指向 `muninn permission-mcp` 子程序（stdio MCP），
  子程序透過 `state_dir/permission.sock` 把請求轉給主程序決策。

## Telegram（M5）

binding 設 `"platform": "telegram"`，`channel_id` 填 chat id（私訊是正數、群組是負數），
token 放在 `telegram_token_env` 指定的環境變數（預設 `TELEGRAM_BOT_TOKEN`）。

- 用 long polling 收訊息，不用對外開 port 或設 webhook。同一個 bot 只能有一個程序在收，跟別的程序共用會 409 衝突。
- 群組要先在 @BotFather `/setprivacy` 關掉隱私模式，bot 才收得到一般訊息。
- 權限確認、定時任務、活動旁白、指令都一樣；指令可以用 `/status`（群組裡 `/status@bot名` 也行）。
- 白名單用 Telegram 的 user id（跟 Discord 的是兩套）。
- 回覆裡的 Markdown（粗體、行內 code、code block、連結）會轉成 Telegram HTML，轉失敗就退回純文字。

## 附件

- 使用者傳的圖片、檔案（Discord 附件；Telegram 照片取最大尺寸、文件）會在處理時下載到
  `state_dir/bindings/<name>/attachments/<job>/`，訊息後面附上路徑，claude 用 Read 讀（圖片也能直接看）。
  只有附件沒有文字的訊息也會處理。單檔上限 25MB（Telegram bot 只能下載 20MB 以內），保留 7 天。
- claude 要把檔案給使用者時用 `send_file` 工具（Discord 上限 8MB；Telegram 50MB，圖片直接顯示）。
- Telegram 的輪詢位置存在 `state_dir/telegram_offset`，重啟不會重收已處理過的訊息。

## 頻道裡的 Muninn 指令（M4）

`!` 開頭、白名單內的人才能下，立刻執行、不交給 claude、不用等目前這一輪跑完：

- `!status`：claude 程序狀態（執行中／閒置多久／沒在跑）、pid、記憶體、排隊中的訊息、等確認的權限
- `!cancel`：中斷 claude 正在跑的這一輪，session 保留
- `!restart`：中斷並關掉 claude 程序，下一則訊息會重新啟動並 `--resume`
- `!schedules`：列出這個頻道的定時任務
- `!help`

## 定時任務（M4）

直接在頻道跟 claude 說就好，例如「平日早上 9 點提醒我看 CI」「明天下午三點提醒我開會」。
Muninn 會給每個頻道裡的 claude 一組工具，管理**自己這個頻道**的任務：

| 工具 | 用途 |
|---|---|
| `schedule_add` | 建立任務：重複的給 `cron`（標準 5 欄，也接受 `@daily`、`MON-FRI`），單次的給 `at`（觸發後自動刪除） |
| `schedule_list` | 列出任務與下次觸發時間（唯讀，不用確認） |
| `schedule_remove` / `schedule_pause` | 刪除／暫停、恢復 |
| `schedule_run_now` | 立刻觸發一次，測試用 |

- 時間到就把任務內容當成一則訊息送進這個頻道的對話（標明是哪個任務觸發的），頻道上會出現「⏰ 定時任務 `名稱` 觸發」。
- 建立、刪除、暫停都會在頻道貼一行通知；這些工具跟其他工具一樣會先跳權限確認（`schedule_list` 除外）。
- `catch_up`：Muninn 沒在跑時錯過的時間點，回來補跑一次（只補最近一次）；不補跑的錯過超過 5 分鐘就跳過。單次任務一律補跑。
- 同一個排定時間點只會執行一次（崩潰、重啟都不會重複）。排程訊息不受白名單限制，也**不會**拿去回權限確認。
- 任務存在 `state_dir/bindings/<name>/schedules.json`。預設時區由設定檔的 `timezone` 決定。
- 頻道裡打 `!schedules` 看清單；`muninn schedules` 在終端機列出所有頻道的任務。

## 活動旁白（M5）

claude 在跑的時候，頻道上會有一則「⏳ 工作中」訊息，列出它正在用的工具（最近 8 步，路徑顯示成相對工作目錄），
最多每 2 秒更新一次；跑完改成「✅ 完成，N 個步驟，耗時 Xs」（或「已中斷」「出錯」）。
純聊天、沒用到工具的回合不會出現。binding 設 `"activity": false` 可以關掉。

## 自我保護（M4）

- **閒置回收**：claude 程序閒置超過 `idle_timeout`（預設 30m，`"0"` 關閉）就關掉，下一則訊息再 `--resume`。
- **記憶體上限**：claude 和它的子程序加總超過 `memory_limit_mb`（預設 2048，`0` 關閉）時，等這一輪跑完就重開。
- **崩潰回放上限**：每則訊息送進 claude 前先記一次嘗試；同一則已經試了 `max_attempts` 次（預設 3）都沒跑完，
  就放棄並在頻道說明，避免「這則訊息讓 Muninn 崩潰 → 重啟回放 → 再崩潰」的無限迴圈。正常關機不算一次。
- **卡住偵測**：claude 跑一輪時超過 `stall_timeout`（預設 20m，`"0"` 關閉）沒有任何輸出、也沒有在等權限確認，
  就判定卡住並中斷這一輪（session 保留），頻道會收到通知。
- **登入失效**：claude 回「Not logged in · Please run /login」這類錯誤時，頻道收到一次通知，暫停送訊息給 claude，
  訊息留在佇列（不算嘗試次數），每 5 分鐘重試；恢復後通知並接著處理。
- Muninn 主程序本身建議用 systemd 跑，並設 `Restart=on-failure`、`MemoryMax=`。

測試：

```sh
go test -race ./...                                               # 單元測試（假 claude、暫存 git repo）
go test -tags integration -run Integration -v ./internal/runner/ ./internal/permission/  # 打真的 claude，會耗額度
```
