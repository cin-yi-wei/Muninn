package permission

import (
	"strings"
	"testing"
)

func TestSystemPromptKeepsChannelFree(t *testing.T) {
	for _, control := range []bool{false, true} {
		p := SystemPromptFor(control)
		for _, want := range []string{"2 分鐘", "背景", "不要等待或輪詢", "mcp__muninn__delegate"} {
			if !strings.Contains(p, want) {
				t.Fatalf("control=%v 的系統提示缺 %q", control, want)
			}
		}
	}
}
