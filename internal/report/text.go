package report

import "strings"

// cleanText 把日志派生文本里的 ASCII 控制字符替换为空格。
// HTML 报告还会做 html.EscapeString，但运维报告是纯文本，直接 cat/贴工单时
// 不能让 ESC 等控制字符注入终端。没有控制字符时直接返回原字符串，不分配。
func cleanText(s string) string {
	dirty := false
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			dirty = true
			break
		}
	}
	if !dirty {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			c = ' '
		}
		b.WriteByte(c)
	}
	return b.String()
}
