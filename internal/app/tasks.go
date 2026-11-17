package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"muninn/internal/permission"
	"muninn/internal/queue"
	"muninn/internal/tasks"
)

// TaskOps 是委派能力（tasks.Manager 實作）。
type TaskOps interface {
	Delegate(src tasks.Source, in tasks.DelegateInput) (tasks.Task, error)
	Followup(source, id, message string) (tasks.Task, error)
	Cancel(source, id string) (tasks.Task, error)
	Get(source, id string) (tasks.Task, error)
	List(source string) []tasks.Task
}

// TaskAuthor 是任務結果訊息的作者 id。
const TaskAuthor = "tasks"

// DeliverTaskResult 把任務結果寫進 inbox（當成一則訊息交給這個頻道的 claude 處理）。
// 跟排程一樣不走 OnMessage：不受白名單限制，也不會拿去回權限確認。id 用來去重。
func (a *App) DeliverTaskResult(id, text string) error {
	key := unsafeChars.ReplaceAllString(id, "_")
	now := time.Now().UTC()
	job := InboxJob{ID: jobID(now.Format(time.RFC3339Nano), "task-"+key), ChannelID: a.ChannelID, MessageID: "task-" + key,
		AuthorID: TaskAuthor, Content: text, CreatedAt: now.Format(time.RFC3339)}
	if err := a.inbox.Put(job.ID, job); err != nil && !errors.Is(err, queue.ErrExists) {
		return err
	}
	wake(a.wakeWorker)
	return nil
}

func (a *App) taskSource() tasks.Source {
	return tasks.Source{Binding: a.Name, Repo: a.Repo, Workdir: a.Workdir}
}

// taskTool 處理 delegate 與 task_* 工具。
func (a *App) taskTool(method string, input json.RawMessage) permission.ToolResult {
	if a.Tasks == nil {
		return toolErr("這個頻道沒有啟用委派")
	}
	var in struct {
		Task    string `json:"task"`
		Title   string `json:"title"`
		Role    string `json:"role"`
		Level   string `json:"level"`
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return toolErr("參數格式錯誤：%v", err)
		}
	}
	switch method {
	case "delegate":
		t, err := a.Tasks.Delegate(a.taskSource(), tasks.DelegateInput{Task: in.Task, Title: in.Title, Role: in.Role, Level: in.Level})
		if err != nil {
			return toolErr("委派失敗：%v", err)
		}
		return permission.ToolResult{Text: fmt.Sprintf("已委派任務 %s（%s／%s），在背景執行；做完結果會自動送進這個對話，不用等也不用輪詢。", t.ID, t.Role, t.Level)}
	case "task_list":
		return permission.ToolResult{Text: a.taskList()}
	case "task_status":
		t, err := a.Tasks.Get(a.Name, in.ID)
		if err != nil {
			return toolErr("%v", err)
		}
		b, _ := json.MarshalIndent(t, "", "  ")
		return permission.ToolResult{Text: string(b)}
	case "task_followup":
		t, err := a.Tasks.Followup(a.Name, in.ID, in.Message)
		if err != nil {
			return toolErr("%v", err)
		}
		return permission.ToolResult{Text: "已送出追問給任務 " + t.ID + "，結果會自動送回這個對話"}
	case "task_cancel":
		t, err := a.Tasks.Cancel(a.Name, in.ID)
		if err != nil {
			return toolErr("%v", err)
		}
		return permission.ToolResult{Text: "已取消任務 " + t.ID}
	}
	return toolErr("不認得的工具 %s", method)
}

func (a *App) taskList() string {
	list := a.Tasks.List(a.Name)
	if len(list) == 0 {
		return "這個頻道沒有委派過任務"
	}
	var lines []string
	for i, t := range list {
		if i >= 15 {
			lines = append(lines, fmt.Sprintf("…還有 %d 個較舊的任務", len(list)-i))
			break
		}
		lines = append(lines, "• "+tasks.Describe(t))
	}
	return strings.Join(lines, "\n")
}
