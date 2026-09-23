package studio

// 时间线正文的截取：命令输出只留尾段。

import "strings"

// storeOutputLimit 是时间线里给命令输出留的尾段上限。
const storeOutputLimit = 16 << 10

// tail 保留末尾 n 字节（按 UTF-8 边界），截了就在前面标一句。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	for i := 0; i < len(cut) && i < 4; i++ {
		if cut[i]&0xC0 != 0x80 {
			cut = cut[i:]
			break
		}
	}
	return "…(earlier output truncated)\n" + cut
}

// firstLine 是一段文本的首行（过长截断）。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120]) + "…"
	}
	return s
}
