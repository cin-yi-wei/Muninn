# Muninn

claude_cron 的重寫版：拿掉 tmux，改用 headless `claude -p`（stream-json 進出），
讓每個聊天頻道對應一個常駐的 Claude session，訊息進來跑完回頻道。

名字取自奧丁的記憶渡鴉 Muninn——飛出去打探、回來報告。

設計與決策見 [DESIGN.md](DESIGN.md)。狀態：M1～M4 完成，下一步 M5（Telegram、活動旁白串流）。

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
- `adopt` 之後，下一則訊息前面會自動附上交接說明（告訴 claude 不用再照 claude_cron 的檔案協定回覆）。
  **同一個 session 同時只能有一個程序在寫**：原本的 claude_cron binding 一定要先停掉再 `run`。

權限確認（M3）：claude 要執行需要確認的工具時，Muninn 會把它貼到頻道：

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

## 頻道裡的 Muninn 指令（M4）

`!` 開頭、白名單內的人才能下，立刻執行、不交給 claude、不用等目前這一輪跑完：

- `!status`：claude 程序狀態（執行中／閒置多久／沒在跑）、pid、記憶體、排隊中的訊息、等確認的權限
- `!cancel`：中斷 claude 正在跑的這一輪，session 保留
- `!restart`：中斷並關掉 claude 程序，下一則訊息會重新啟動並 `--resume`
- `!help`

## 定時任務（M4）

格式沿用 claude_cron 的 `triggers.json`，放在 `triggers_file`（預設 `<state_dir>/triggers.json`）：

```json
{"triggers": [{"name": "am-ci", "binding": "muninn-main", "cron": "0 9 * * 1-5", "timezone": "Asia/Taipei",
               "message": "早安，幫我看一下昨天的 CI", "catch_up": false, "enabled": true}]}
```

- 到期就當成使用者在頻道傳了 `message`，走同一條佇列。每 30 秒檢查一次，可以直接手改檔案（Muninn 寫回時只更新 `last_checked`）。
- `catch_up`：Muninn 沒在跑時錯過的時間點，回來補跑一次（`false` 則錯過超過 10 分鐘就跳過）。
- 同一個排定時間點只會執行一次（崩潰、重啟都不會重複）。排程訊息不受白名單限制，也**不會**拿去回權限確認。
- `muninn triggers list`：列出所有任務與下次觸發時間。`muninn check` 會驗證 cron、時區、binding。
- `muninn triggers import <claude_cron 的 triggers.json>`：只匯入本設定有的 binding，不補跑過去的時間點。
  claude_cron 那邊同名的若還開著，兩邊會各觸發一次。

## 自我保護（M4）

- **閒置回收**：claude 程序閒置超過 `idle_timeout`（預設 30m，`"0"` 關閉）就關掉，下一則訊息再 `--resume`。
- **記憶體上限**：claude 和它的子程序加總超過 `memory_limit_mb`（預設 2048，`0` 關閉）時，等這一輪跑完就重開。
- **崩潰回放上限**：每則訊息送進 claude 前先記一次嘗試；同一則已經試了 `max_attempts` 次（預設 3）都沒跑完，
  就放棄並在頻道說明，避免「這則訊息讓 Muninn 崩潰 → 重啟回放 → 再崩潰」的無限迴圈。正常關機不算一次。
- Muninn 主程序本身建議用 systemd 跑，並設 `Restart=on-failure`、`MemoryMax=`。

測試：

```sh
go test -race ./...                                               # 單元測試（假 claude、暫存 git repo）
go test -tags integration -run Integration -v ./internal/runner/ ./internal/permission/  # 打真的 claude，會耗額度
```
