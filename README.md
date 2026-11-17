# Muninn

claude_cron 的重寫版：拿掉 tmux，改用 headless `claude -p`（stream-json 進出），
讓每個聊天頻道對應一個常駐的 Claude session，訊息進來跑完回頻道。

名字取自奧丁的記憶渡鴉 Muninn——飛出去打探、回來報告。

設計與決策見 [DESIGN.md](DESIGN.md)。狀態：M1 實作中。

## M1：單頻道跑起來

```sh
go build -o bin/muninn ./cmd/muninn

DISCORD_BOT_TOKEN=... \
MUNINN_CHANNEL_ID=<頻道 id> \
MUNINN_ALLOWED_USERS=<你的 user id> \
MUNINN_WORKDIR=/path/to/repo \
./bin/muninn
```

其他選填環境變數見 `cmd/muninn/main.go` 開頭註解。狀態（inbox/outbox 佇列、session id）
寫在 `./state/`；崩潰重啟會回放 pending 裡的 job，並用 `--resume` 接回同一個 session。

M1 還沒有權限 MCP（M3），claude 在 `-p` 模式下碰到需要確認的工具會直接被拒。
要先放行可設 `MUNINN_CLAUDE_ARGS="--permission-mode acceptEdits"` 之類。

測試：

```sh
go test -race ./...                                          # 單元測試（假 claude）
go test -tags integration -run Integration -v ./internal/runner/  # 打真的 claude，會耗額度
```
