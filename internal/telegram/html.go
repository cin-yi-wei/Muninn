package telegram

import (
	"html"
	"regexp"
	"strings"
)

// ToHTML 把 Muninn 與 claude 慣用的 Markdown 子集轉成 Telegram 的 HTML：
//
//	```lang\n…\n``` → <pre><code class="language-lang">…</code></pre>（沒有標記就不帶 class）
//	`code`          → <code>…</code>
//	**粗體**        → <b>…</b>
//	[文字](網址)     → <a href="網址">文字</a>
//
// 其他字一律跳脫。轉不好的情況（例如標記沒成對）由呼叫端在 Telegram 拒絕時退回純文字。
func ToHTML(md string) string {
	var b strings.Builder
	for {
		i := strings.Index(md, "```")
		if i < 0 {
			b.WriteString(inline(md))
			return b.String()
		}
		j := strings.Index(md[i+3:], "```")
		if j < 0 { // 沒收尾的 code block：剩下的照一般文字處理
			b.WriteString(inline(md))
			return b.String()
		}
		b.WriteString(inline(md[:i]))
		block := md[i+3 : i+3+j]
		// 第一行若是語言標記（沒有空白）就拿掉；標記合法才放進 class，讓用戶端依語言上色
		open := "<pre><code>"
		if nl := strings.IndexByte(block, '\n'); nl >= 0 && !strings.ContainsAny(block[:nl], " \t") {
			if lang := block[:nl]; langRE.MatchString(lang) {
				open = `<pre><code class="language-` + lang + `">`
			}
			block = block[nl+1:]
		}
		block = strings.TrimSuffix(block, "\n")
		b.WriteString(open + html.EscapeString(block) + "</code></pre>")
		md = md[i+3+j+3:]
	}
}

var (
	// langRE 是允許放進 class 的語言標記：只收英數與 -_+，其他字元一律當成沒標記，避免注入屬性
	langRE = regexp.MustCompile(`^[A-Za-z0-9_+-]+$`)
	codeRE = regexp.MustCompile("`([^`\n]+)`")
	boldRE = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	linkRE = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
)

// inline 處理一段不含 code block 的文字。行內 code 先抽出來保護，避免裡面的 ** 被當粗體。
func inline(s string) string {
	var codes []string
	s = codeRE.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, html.EscapeString(m[1:len(m)-1]))
		return "\x00" + string(rune('A'+len(codes)-1)) + "\x00"
	})
	s = html.EscapeString(s)
	s = linkRE.ReplaceAllString(s, `<a href="$2">$1</a>`)
	s = boldRE.ReplaceAllString(s, "<b>$1</b>")
	for i, c := range codes {
		s = strings.Replace(s, "\x00"+string(rune('A'+i))+"\x00", "<code>"+c+"</code>", 1)
	}
	return s
}
