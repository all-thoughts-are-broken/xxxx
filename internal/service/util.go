package service

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
)

// mustJSON 把 v 序列化成 json.RawMessage。
//
// 只用于「参数完全由本包构造、不可能序列化失败」的场景（map[string]any 里
// 装的都是 string），所以失败时返回 nil 而不 panic —— config.Set 收到 nil
// 会给出明确报错，比在业务代码里 panic 更容易定位。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// sanitizeFileName 把任意文本（章节名/作品名）转成安全的文件名。
//
// Windows 与 POSIX 的非法字符取并集，另外处理几处容易踩的点：
// 控制字符、首尾空白、路径分隔符、Windows 上不允许出现在文件名末尾的点与空格，
// 以及「清洗后只剩占位符」这种等于没有名字的情况。
//
// 返回空串表示「这个名字不可用」，调用方应当走兜底命名（见 safeChapterFileName）。
func sanitizeFileName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	const maxRunes = 80 // 给扩展名与目录前缀留余量

	var b strings.Builder
	b.Grow(len(s))

	count := 0
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f:
			// 控制字符直接丢弃
			continue
		default:
			b.WriteRune(r)
		}
		count++
		if count >= maxRunes {
			break
		}
	}

	// TrimRight 而非 TrimSpace：首部空白已被开头的 TrimSpace 处理掉，
	// 这里只额外去掉 Windows 不接受的结尾点与空格。
	out := strings.TrimRight(b.String(), " .")
	if out == "" || isOnlySeparators(out) {
		return ""
	}
	// 设备名冲突（Windows 上 CON/PRN/NUL 等不能作文件名）
	if isWindowsReserved(out) {
		out = "_" + out
	}
	return out
}

// isOnlySeparators 报告字符串是否只剩占位符/分隔符，即没有任何实际内容。
//
// 例如 "///" 清洗成 "___"：它是个合法文件名，但毫无意义，
// 用它当章节名不如让调用方走 "chapter_<id>" 兜底。
func isOnlySeparators(s string) bool {
	for _, r := range s {
		switch r {
		case '_', '.', '-', ' ', '\t':
		default:
			return false
		}
	}
	return true
}

// isWindowsReserved 判断是否为 Windows 保留设备名（不区分大小写，忽略扩展名）。
func isWindowsReserved(name string) bool {
	base := strings.ToUpper(name)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// safeChapterFileName 生成章节文件的基名：优先用章节名，为空时退回 id。
func safeChapterFileName(name string, id int) string {
	if base := sanitizeFileName(name); base != "" {
		return base
	}
	return "chapter_" + itoa(id)
}

// itoa 是 strconv.Itoa 的短别名，仅用于本文件的字符串拼接，避免到处 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// isProbablyHTMLTitle 判断文件名是否看起来像被当成了 HTML 标题（用于封面兜底）。
//
// 这里只是给 sanitizeFileName 之外做一层保险：CDN 出错时可能返回 HTML，
// 若上游把页面标题当成了文件名，会产生一堆奇怪字符。
func isProbablyHTMLTitle(s string) bool {
	t := strings.ToLower(s)
	return strings.Contains(t, "<!doctype") || strings.Contains(t, "<html")
}

// ensureExt 确保路径带指定扩展名（大小写不敏感）。
func ensureExt(path, ext string) string {
	if strings.EqualFold(filepath.Ext(path), ext) {
		return path
	}
	return path + ext
}

// trimUnicodeSpace 去掉首尾的 Unicode 空白（含全角空格 U+3000）。
//
// 保留这个命名只是为了在调用点表达意图（"这里是清接口给的脏空白"），
// 它与 strings.TrimSpace **完全等价** —— 后者的实现就是
// TrimFunc(s, unicode.IsSpace)，而 unicode.IsSpace 对 U+3000 返回 true。
//
// ⚠️ 这里原先的注释写着"用 strings.TrimSpace 去不掉"，是错的（2026-09-21 实测：
// unicode.IsSpace('\u3000') == true，TrimSpace 能去掉全角空格、NBSP、EM SPACE）。
// 保留一句提醒：别照着旧注释去"加强" TrimSpace 的替代品。
func trimUnicodeSpace(s string) string {
	return strings.TrimFunc(s, unicode.IsSpace)
}
