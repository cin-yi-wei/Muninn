# Muninn

claude_cron 的重寫版：拿掉 tmux，改用 headless `claude -p`（stream-json 進出），
讓每個聊天頻道對應一個常駐的 Claude session，訊息進來跑完回頻道。

名字取自奧丁的記憶渡鴉 Muninn——飛出去打探、回來報告。

設計與決策見 [DESIGN.md](DESIGN.md)。狀態：M1 完成，M2 實作中。

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

還沒有權限 MCP（M3），claude 在 `-p` 模式下碰到需要確認的工具會直接被拒。
要先放行可在 binding 加 `"claude_args": ["--permission-mode", "acceptEdits"]`。

測試：

```sh
go test -race ./...                                               # 單元測試（假 claude、暫存 git repo）
go test -tags integration -run Integration -v ./internal/runner/  # 打真的 claude，會耗額度
```
