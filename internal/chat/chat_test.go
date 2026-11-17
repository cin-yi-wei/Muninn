package chat

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitKeepsFences(t *testing.T) {
	code := "```go\n" + strings.Repeat("fmt.Println(1)\n", 300) + "```\n結尾"
	parts := SplitMessage("說明\n"+code, 2000)
	if len(parts) < 2 {
		t.Fatalf("應切成多段：%d", len(parts))
	}
	for i, p := range parts {
		if strings.Count(p, "```")%2 != 0 {
			t.Fatalf("第 %d 段圍欄沒成對：%q…", i, p[:40])
		}
		if n := utf8.RuneCountInString(p); n > 2000 {
			t.Fatalf("第 %d 段超長：%d", i, n)
		}
	}
	if !strings.HasPrefix(parts[1], "```go\n") {
		t.Fatalf("下一段應重新開啟同語言的區塊：%q", parts[1][:20])
	}
	if !strings.HasSuffix(parts[len(parts)-1], "結尾") {
		t.Fatal("最後一段內容不對")
	}
}

func TestSplitShort(t *testing.T) {
	if p := SplitMessage("hi", 2000); len(p) != 1 || p[0] != "hi" {
		t.Fatal(p)
	}
	if p := SplitMessage("", 2000); p[0] == "" {
		t.Fatal("空字串應有預設文字")
	}
}
