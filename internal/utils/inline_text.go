package utils

import (
	"image"
	"math"
	"regexp"
	"strings"

	"github.com/fogleman/gg"
)

// ---------------- 评论正文的行内富文本（文字 + 表情图混排） ----------------
//
// 评论正文是 HTML 片段，里面除了文字还有两种「表情」，**都是图片、都不是字体问题**：
//
//  1. 服务端表情面板插入的贴纸，正文里直接是
//     `<img src="https://www.cdnbea.net/media/emoji/<sha1>.png" alt="惊喜">`
//  2. 用户手打的 Unicode 码位（`😋`、`❤️`、`👍🏽`）
//
// 老实现用 stripHTML 把标签一律抹掉，于是：
//   - 只发表情的评论被 wash 成空串，落进「这条评论没有内容」兜底分支；
//   - 夹在文字里的表情整段消失，一个字都看不到。
//
// 而 Noto Sans SC 连 emoji 码位都没有（实测 glyphIndex=0，画出来是个 □），
// 所以第 2 类也不能靠字体。这里的做法是把两类都归一到「一段文字 + 若干张图」，
// 交给同一套混排逻辑：图片与文字共用一个行盒，逐字换行时把图当成一个不可拆的原子。

// TwemojiBaseURL 是 Unicode 码位映射到的彩色表情图源。
//
// 选它是因为它是社区事实标准（native 版参考实现同样抓它），72x72 PNG 体积小、
// 且文件名规则稳定可枚举。@2x 目录是 36x36，不用。
const TwemojiBaseURL = "https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/72x72"

// TwemojiHost 是 Twemoji 的 CDN 域名。
//
// 单独拎出来是因为它**不能参与"换镜像域名重试"**：那套机制只在 JM 自己的图片
// CDN 之间换（路径与域名无关），把它套到第三方 CDN 上只会白等一轮失败。
const TwemojiHost = "cdnjs.cloudflare.com"

// TwemojiURL 返回某个 emoji key 对应的图片直链。
func TwemojiURL(key string) string { return TwemojiBaseURL + "/" + key + ".png" }

// inlineRunKind 区分行内 run 的来源。
type inlineRunKind int

const (
	// inlineTextRun 普通文字。
	inlineTextRun inlineRunKind = iota
	// inlineStickerRun 服务端贴纸（正文里的 <img>）。
	inlineStickerRun
	// inlineEmojiRun Unicode emoji 码位（映射到 Twemoji）。
	inlineEmojiRun
)

// inlineRun 是正文切出来的行内单元。
type inlineRun struct {
	kind inlineRunKind
	// text inlineTextRun 的文字；inlineEmojiRun 退回原始码位（图取不到时兜底画它）。
	text string
	// url 图片直链：贴纸用正文里的 src，emoji 用 Twemoji 直链。
	url string
	// key Twemoji 文件名（不含扩展名），仅 inlineEmojiRun 有值。
	key string
	// alt 贴纸的 alt 文本（图取不到时兜底成 [alt]）。
	alt string
}

var (
	imgTagRe = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	// 属性取值三种引号形态都要认：服务端用的是双引号，但历史数据里混着单引号。
	// 属性名前面要求「行首或空白」，否则 `data-src=` 里的 `src` 也会被匹配上
	// （`-` 与 `s` 之间正好是词边界，`\bsrc` 拦不住）。
	imgSrcRe = regexp.MustCompile(`(?is)(?:^|\s)src\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	imgAltRe = regexp.MustCompile(`(?is)(?:^|\s)alt\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// InlineImage 是正文里引用到的一处内联图。
type InlineImage struct {
	// URL 图片直链，同时充当「预抓 → 渲染」之间查本地缓存的键。
	URL string
	// Alt 贴纸的 alt 文本，可能为空。
	Alt string
	// Sticker 为 true 表示这是服务端贴纸（可以换镜像域名重试），
	// false 表示 Unicode 码位映射出来的 Twemoji（第三方 CDN，不换域名）。
	Sticker bool
}

// CollectInlineImages 返回正文引用到的全部内联图（按出现顺序、已按 URL 去重）。
//
// 给服务层预抓用：渲染层是纯计算的（没有网络），图必须先落盘再喂进去。
func CollectInlineImages(content string) []InlineImage {
	runs := tokenizeInline(content)
	out := make([]InlineImage, 0, len(runs))
	seen := make(map[string]bool, len(runs))
	for _, r := range runs {
		if r.url == "" || seen[r.url] {
			continue
		}
		seen[r.url] = true
		out = append(out, InlineImage{
			URL:     r.url,
			Alt:     r.alt,
			Sticker: r.kind == inlineStickerRun,
		})
	}
	return out
}

// tokenizeInline 把一段正文切成行内 run 序列：先把 <img> 摘出来，其余当文字，
// 文字再按 emoji 码位切开。
func tokenizeInline(content string) []inlineRun {
	if strings.TrimSpace(content) == "" {
		return nil
	}

	var runs []inlineRun
	last := 0
	for _, loc := range imgTagRe.FindAllStringIndex(content, -1) {
		runs = append(runs, textRuns(stripHTML(content[last:loc[0]]))...)

		tag := content[loc[0]:loc[1]]
		// 没有 src 的 <img> 什么也画不出来，直接丢掉（不留空 run）。
		if src := firstAttr(imgSrcRe, tag); src != "" {
			runs = append(runs, inlineRun{kind: inlineStickerRun, url: src, alt: firstAttr(imgAltRe, tag)})
		}
		last = loc[1]
	}
	runs = append(runs, textRuns(stripHTML(content[last:]))...)
	return runs
}

// textRuns 把纯文本按 Unicode emoji 码位切开。
func textRuns(text string) []inlineRun {
	if text == "" {
		return nil
	}
	chars := []rune(text)
	runs := make([]inlineRun, 0, 4)
	var buf []rune
	flush := func() {
		if len(buf) > 0 {
			runs = append(runs, inlineRun{kind: inlineTextRun, text: string(buf)})
			buf = buf[:0]
		}
	}

	for i := 0; i < len(chars); {
		if !isEmojiStart(chars[i]) {
			buf = append(buf, chars[i])
			i++
			continue
		}
		flush()
		key, used := emojiKey(chars[i:])
		runs = append(runs, inlineRun{
			kind: inlineEmojiRun,
			text: string(chars[i : i+used]),
			key:  key,
			url:  TwemojiURL(key),
		})
		i += used
	}
	flush()
	return runs
}

// emojiKey 计算 Twemoji 的文件名（不含扩展名）。
//
// 规则是**对着 cdnjs 上的 14.0.2 目录实测**出来的（别照抄 native 版实现）：
//   - 码位用小写十六进制、**不加前导零**，`-` 连接：`1f603`、`1f44d-1f3fb`、`31-20e3`
//     （`0031-20e3.png` 是 404，Twemoji 不带前导零）
//   - **变体选择符 FE0E/FE0F 一律去掉**：官方目录里没有 `2764-fe0f.png`，
//     只有 `2764.png`；带 FE0F 的 key 会 404（native 版把 FE0F 编进 key，
//     于是 ❤️ 这类基本都会掉进 □ 兜底）
//   - ZWJ(200d) 与肤色修饰符保留，作为序列的一部分：`1f468-200d-1f469-200d-1f467`
//
// 返回 key 与消耗的码位数（至少 1，保证调用方一定前进，不会死循环）。
func emojiKey(chars []rune) (string, int) {
	if len(chars) == 0 {
		return "", 1
	}
	cps := []string{hexCP(chars[0])}
	used := 1

	// 国旗是「两个区域指示符」的固定组合（🇨🇳 = 1F1E8 1F1F3），
	// 不在下面的修饰符循环里，单独接一下；漏了会 404 成两个 □。
	if isRegionalIndicator(chars[0]) && len(chars) > 1 && isRegionalIndicator(chars[1]) {
		return cps[0] + "-" + hexCP(chars[1]), 2
	}

loop:
	for used < len(chars) {
		ch := chars[used]
		switch {
		case isVariationSelector(ch):
			// 只消费、不进 key —— 见上面实测规则第 2 条。
			used++
		case isEmojiModifier(ch):
			cps = append(cps, hexCP(ch))
			used++
		case ch == '\u200d' && used+1 < len(chars) && isEmojiStart(chars[used+1]):
			cps = append(cps, "200d", hexCP(chars[used+1]))
			used += 2
		default:
			break loop
		}
	}
	return strings.Join(cps, "-"), used
}

func hexCP(r rune) string {
	const digits = "0123456789abcdef"
	if r == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for r > 0 {
		i--
		buf[i] = digits[r&0xf]
		r >>= 4
	}
	return string(buf[i:])
}

// isEmojiStart 判定「可以起一个 emoji 序列」的码位范围。
//
// 前三段与 native 版参考实现一致（最常用的三大块）。后面几段是**故意多加**的
// 「单码位 emoji 区」：参考实现漏了 ⭐(2B50)、⭕(2B55)、⬛(2B1B)、▪(25AA)、
// ‼(203C)、™(2122) 这些，漏掉的后果是画成 □（主字体没有这些字形）。
//
// 加宽范围是安全的：Twemoji 里没有对应文件时只是 404，排版会退回原字符
// （见 layoutInline 的兜底），不会把普通符号画错。反过来不做字体覆盖检查 ——
// Noto Sans SC 对整片 emoji 区都没有字形，查了也白查，还会因为个别码位
// 恰好有字形（如 →）而与参考实现分叉。
func isEmojiStart(ch rune) bool {
	switch {
	case ch >= 0x1F000 && ch <= 0x1FAFF: // 含区域指示符(国旗)、肤色修饰符、ZWJ 序列
		return true
	case ch >= 0x2600 && ch <= 0x27BF:
		return true
	case ch >= 0x2300 && ch <= 0x23FF:
		return true
	case ch >= 0x2B00 && ch <= 0x2BFF: // ⭐ ⭕ ⬛ ⬜
		return true
	case ch >= 0x25A0 && ch <= 0x25FF: // ▪ ▫ ◻ ◼ ◾
		return true
	case ch == 0x203C || ch == 0x2049: // ‼ ⁉
		return true
	case ch == 0x00A9 || ch == 0x00AE || ch == 0x2122: // © ® ™
		return true
	}
	return false
}

func isVariationSelector(ch rune) bool { return ch >= 0xFE00 && ch <= 0xFE0F }

func isEmojiModifier(ch rune) bool { return ch >= 0x1F3FB && ch <= 0x1F3FF }

func isRegionalIndicator(ch rune) bool { return ch >= 0x1F1E6 && ch <= 0x1F1FF }

// firstAttr 返回标签里第一个属性的值（三种引号形态，取第一个非空捕获组）。
//
// 属性不存在时必须返回空串而不是让切片越界：正文里的 <img> 千奇百怪，
// 没有 alt、甚至没有 src 的都出现过。
func firstAttr(re *regexp.Regexp, tag string) string {
	m := re.FindStringSubmatch(tag)
	if len(m) == 0 {
		return ""
	}
	for _, g := range m[1:] {
		if g != "" {
			return g
		}
	}
	return ""
}

// ---------------- 混排布局 ----------------

// inlineSeg 是排好的一行里的一段：要么文字，要么一张图。
type inlineSeg struct {
	text string
	img  image.Image
	// w 该段的逻辑宽度（图片按缩放后的位图宽度换算，保证与绘制一致）。
	w float64
}

// inlineLine 是一行：若干段 + 总宽。
type inlineLine struct {
	segs []inlineSeg
	w    float64
}

// InlineImageResolver 把一处内联图地址解析成可绘制的位图。
//
// 返回的位图要求**已是设备分辨率且已缩放到 inlineEmojiSize(size) 见方**
// （与本包其它绘制函数一致：布局用逻辑坐标，位图按 renderScale 预放大）。
// 返回 nil 表示这张图拿不到，排版会退回文字兜底。
type InlineImageResolver func(url, alt string) image.Image

// inlineEmojiSize 返回表情图在行内的边长（逻辑单位）。
//
// 1.12 取自 native 版参考实现（emoji_advance）；比字号略大一点，
// 这样表情与汉字视觉重量相当。
func inlineEmojiSize(size float64) float64 { return math.Ceil(size * 1.12) }

// layoutInline 把正文排成若干行，每行是文字/图片分段的序列。
//
// 换行规则与 wrapText 一致（逐字贪心、行尾去空格、行首空格丢弃），
// 差别只在于：图片是一个不可拆的原子，放不下就整体挪到下一行。
// maxLines > 0 时超出部分截断并在末行补省略号。
func layoutInline(
	content string, maxWidth, textSize float64, weight, maxLines int, resolve InlineImageResolver,
) []inlineLine {
	runs := tokenizeInline(content)
	if len(runs) == 0 {
		return nil
	}

	var (
		lines []inlineLine
		cur   inlineLine
		buf   strings.Builder
		bufW  float64
	)

	commitText := func() {
		if buf.Len() == 0 {
			return
		}
		cur.segs = append(cur.segs, inlineSeg{text: buf.String(), w: bufW})
		cur.w += bufW
		buf.Reset()
		bufW = 0
	}
	// wrap 结束当前行（把挂着的文字先落地，避免"有文字的行被当成空行丢掉"）。
	wrap := func() {
		commitText()
		if len(cur.segs) > 0 {
			lines = append(lines, cur)
			cur = inlineLine{}
		}
	}
	// pushText 把一个字符串按字喂进当前行；图片兜底也走它。
	pushText := func(s string) {
		for _, ch := range s {
			if ch == ' ' && buf.Len() == 0 && len(cur.segs) == 0 {
				continue // 行首空格丢掉（与 wrapText 一致）
			}
			w := measureTextW(buf.String()+string(ch), textSize, weight)
			if cur.w+w > maxWidth && (buf.Len() > 0 || len(cur.segs) > 0) {
				wrap()
				if ch == ' ' {
					continue
				}
				w = measureTextW(string(ch), textSize, weight)
			}
			bufW = w
			buf.WriteRune(ch)
		}
	}

	for _, run := range runs {
		if run.kind == inlineTextRun {
			pushText(run.text)
			continue
		}

		img := image.Image(nil)
		if resolve != nil {
			img = resolve(run.url, run.alt) // resolve 为 nil = 一张图都没有（单测常用）
		}
		if img == nil {
			// 图拿不到：emoji 退回原始码位（主字体至少能画个 □，与参考实现一致），
			// 贴纸退回 [alt]。**关键是不能当它不存在** —— 纯表情评论会因此
			// 被判成"没有内容"，正是这次要修的 bug。
			if run.kind == inlineEmojiRun {
				pushText(run.text)
			} else {
				pushText(stickerFallback(run.alt))
			}
			continue
		}

		b := img.Bounds()
		w := float64(b.Dx()) / renderScale
		// 判溢出时要把**还挂在 buf 里没落地的文字**算进去：cur.w 只统计已提交的
		// 段，只看它会让"文字刚好排满一行 + 后面跟个表情"这行溢出（实测漏过）。
		if cur.w+bufW+w > maxWidth && (len(cur.segs) > 0 || buf.Len() > 0) {
			wrap()
		} else {
			commitText() // 图必须画在已有文字之后，先把它落地
		}
		cur.segs = append(cur.segs, inlineSeg{img: img, w: w})
		cur.w += w
	}
	wrap()

	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		last := &lines[maxLines-1]
		ellipsis := "…"
		w := measureTextW(ellipsis, textSize, weight)
		last.segs = append(last.segs, inlineSeg{text: ellipsis, w: w})
		last.w += w // 行宽要跟着更新，否则气泡宽度会按截断前的老宽度算
	}
	return lines
}

// stickerFallback 是贴纸取不到图时的占位文字。
func stickerFallback(alt string) string {
	if strings.TrimSpace(alt) == "" {
		return "[表情]"
	}
	return "[" + strings.TrimSpace(alt) + "]"
}

// maxInlineW 返回所有行里最宽的一行（逻辑宽度）。
func maxInlineW(lines []inlineLine) float64 {
	var m float64
	for _, ln := range lines {
		if ln.w > m {
			m = ln.w
		}
	}
	return m
}

// inlineLinesText 返回每行的纯文字（测试与调试用；图片不参与）。
func inlineLinesText(lines []inlineLine) []string {
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		var sb strings.Builder
		for _, seg := range ln.segs {
			if seg.img != nil {
				sb.WriteString("{img}")
				continue
			}
			sb.WriteString(seg.text)
		}
		out = append(out, sb.String())
	}
	return out
}

// drawInlineLine 画一行：文字走 drawGlyph，图片与文字着墨中心对齐。
//
// yTop / lineH 是这一行的行盒（与 drawText 同一套坐标）；文字统一用
// baseline = yTop + ascent，图片则居中在**着墨中心**上 —— 汉字墨迹只占
// ascent 的下半截，直接按行盒居中会让表情明显偏上（见 inkCenterOffset）。
func drawInlineLine(
	dc *gg.Context, ln inlineLine, x, yTop, textSize float64, hex string, weight int,
) {
	if len(ln.segs) == 0 {
		return
	}
	baseline := yTop + fontAscent(textSize, isBold(weight))
	inkCenter := baseline - inkCenterOffset(textSize)

	cursor := x
	for _, seg := range ln.segs {
		if seg.img != nil {
			h := float64(seg.img.Bounds().Dy()) / renderScale
			paste(dc, seg.img, cursor, inkCenter-h/2)
			cursor += seg.w
			continue
		}
		setText(dc, textSize, hex, weight)
		drawGlyph(dc, seg.text, cursor, baseline)
		cursor += seg.w
	}
}

// inkCenterOffset 返回汉字着墨中心相对 baseline 的偏移（逻辑单位）。
//
// 实测 Noto Sans SC：汉字墨迹顶约在 baseline 上方 0.88em、底约在下方 0.12em，
// 所以着墨中心 ≈ baseline - 0.38em。hhea ascent≈1.16em，里面有一大截是给
// 注音/上级符号留的空档，拿 ascent 的中点当视觉中心会让表情偏上近 0.1em。
func inkCenterOffset(size float64) float64 { return 0.38 * size }
