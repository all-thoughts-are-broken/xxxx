package utils

import (
	"fmt"
	"html"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/disintegration/imaging"
	"github.com/fogleman/gg"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// ---------------- 渲染倍率（超采样） ----------------
//
// 所有布局/字号都按 1x 逻辑坐标设计，实际画布按 renderScale 放大。
// 注意：gg 的 DrawString 是"先按 face 原始尺寸光栅化字形位图，再双线性贴上"，
// 所以 dc.Scale 对文字无效（位图会被二次插值变糊）；文字必须让 face 按
// size*renderScale 光栅化，再通过临时抵消矩阵用设备坐标绘制（drawGlyph）。
var renderScale = 2.0

// ---------------- 字体 ----------------
//
// 原实现直接引用 fontRegular / fontBold 这两个变量，但它们从来没有被定义过，
// 导致这个包根本无法编译。这里补上定义，并改成由配置在启动时注入。
var (
	// renderMu 串行化整个渲染过程（详情卡 / 评论图）。
	//
	// 渲染层的字体与画布体系**不是并发安全的**，三处都会出事：
	//   - faceCache / ascentCache / faceCacheDcs / fontFaces 是无锁包级 map，
	//     并发写触发的是运行时的 fatal error: concurrent map writes ——
	//     这是 fatal 不是 panic，invoke 的 recover 接不住，**整个进程直接消失**，
	//     宿主侧表现为所有在途任务一起没了；
	//   - opentype.Face 内部带可变状态（sfnt.Buffer / vector.Rasterizer /
	//     image.Alpha 以及惰性填充的 metrics），跨 goroutine 共用会数据竞态；
	//   - measureCtx 复用同一个 *gg.Context 做文字测量。
	//
	// 协议层默认允许 8 个请求同时在跑（protocol.defaultMaxConcurrency），
	// 宿主完全可以同时下发详情卡与评论图。渲染是"出图"型操作、不在高频路径上，
	// 串行化是最稳的取舍：卡住的是另一个渲染请求，不是整台机器。
	renderMu sync.Mutex

	// fontPathMu 保护下面两个字体路径变量。
	//
	// 单独一把锁（而不是复用 renderMu）是为了让 config_set 改字体路径时
	// 不必等一次渲染跑完——渲染期间只在这两个变量上做极短的读。
	fontPathMu sync.RWMutex

	fontRegular = filepath.Join("fonts", "NotoSansSC-Regular.ttf")
	fontBold    = filepath.Join("fonts", "Noto-Sans-SC-Bold-2.ttf")
)

// SetupFonts 设置渲染用字体路径，应在任何渲染调用之前执行。
//
// 传入空串表示保持原值。字体文件不存在时不在这里报错，而是留到渲染时由
// FontsReady 判断——这样"只想生成 PDF、不需要渲染图文卡片"的场景不会被拖住。
func SetupFonts(regular, boldPath string) {
	fontPathMu.Lock()
	defer fontPathMu.Unlock()
	if regular != "" {
		fontRegular = regular
	}
	if boldPath != "" {
		fontBold = boldPath
	}
}

// FontPaths 返回当前生效的字体路径。
func FontPaths() (regular, bold string) {
	fontPathMu.RLock()
	defer fontPathMu.RUnlock()
	return fontRegular, fontBold
}

// fontPaths 在持锁状态下取一次快照，供内部热路径使用（避免每个字形都加锁）。
func fontPaths() (regular, bold string) {
	fontPathMu.RLock()
	defer fontPathMu.RUnlock()
	return fontRegular, fontBold
}

// FontsReady 报告字体文件是否都可读。
func FontsReady() bool {
	regular, bold := fontPaths()
	for _, p := range []string{regular, bold} {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			return false
		}
	}
	return true
}

// newCanvas 创建放大后的画布，并把坐标系缩放回逻辑单位。
func newCanvas(w, h float64) *gg.Context {
	dc := gg.NewContext(int(math.Round(w*renderScale)), int(math.Round(h*renderScale)))
	dc.Scale(renderScale, renderScale)
	return dc
}

// drawGlyph 以设备分辨率绘制文字（face 已按 size*scale 加载），坐标为逻辑单位。
func drawGlyph(dc *gg.Context, text string, xLogical, baselineLogical float64) {
	dc.Push()
	dc.Scale(1/renderScale, 1/renderScale) // 抵消矩阵 → DrawString 拿到的是设备坐标，字形 1:1 落地无插值
	dc.DrawString(text, xLogical*renderScale, baselineLogical*renderScale)
	dc.Pop()
}

// paste 把一张按设备分辨率准备好的位图贴到逻辑坐标 (x, y)。
func paste(dc *gg.Context, img image.Image, xLogical, yLogical float64) {
	dc.Push()
	dc.Scale(1/renderScale, 1/renderScale)
	dc.DrawImageAnchored(img, int(math.Round(xLogical*renderScale)), int(math.Round(yLogical*renderScale)), 0, 0)
	dc.Pop()
}

type faceKey struct {
	size  float64
	bold  bool
	scale float64
}

var (
	faceCache    = map[faceKey]font.Face{}
	ascentCache  = map[faceKey]float64{}
	faceCacheDcs = map[float64]*gg.Context{} // 借用一个 ctx 来测量
)

func measureCtx() *gg.Context {
	dc, ok := faceCacheDcs[1]
	if !ok {
		dc = gg.NewContext(1, 1)
		faceCacheDcs[1] = dc
	}
	return dc
}

func isBold(weight int) bool { return weight >= 600 }

// fontFaces 缓存解析后的字体（Parse 开销大，按路径缓存）。
var fontFaces = map[string]*opentype.Font{}

func parseFont(path string) *opentype.Font {
	if f, ok := fontFaces[path]; ok {
		return f
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("读取字体失败 %s: %v", path, err))
	}
	ft, err := opentype.Parse(raw)
	if err != nil {
		panic(fmt.Sprintf("解析字体失败 %s: %v", path, err))
	}
	fontFaces[path] = ft
	return ft
}

// face 用 x/image opentype 而不是 gg 自带的 freetype：
// 这套 Noto 字体轮廓带重叠部件，freetype 按奇偶规则填充，笔画交叉处会
// 相互抵消出白色空洞；opentype 用非零环绕规则，无此问题。
func face(size float64, bold bool) font.Face {
	k := faceKey{size, bold, renderScale}
	f, ok := faceCache[k]
	if !ok {
		regular, boldPath := fontPaths()
		path := regular
		if bold {
			path = boldPath
		}
		// 按 设备像素尺寸 光栅化（size 为逻辑字号）
		var err error
		f, err = opentype.NewFace(parseFont(path), &opentype.FaceOptions{
			Size:    size * renderScale,
			DPI:     72,
			Hinting: font.HintingNone,
		})
		if err != nil {
			panic(fmt.Sprintf("加载字体失败 %s: %v", path, err))
		}
		faceCache[k] = f
	}
	return f
}

func fontAscent(size float64, bold bool) float64 {
	k := faceKey{size, bold, renderScale}
	if v, ok := ascentCache[k]; ok {
		return v
	}
	m := face(size, bold).Metrics()
	a := float64(m.Ascent) / 64 / renderScale // 换算回逻辑单位
	ascentCache[k] = a
	return a
}

// fontDescent 返回逻辑单位下行高（负值），配合 fontAscent 得到行框全高。
func fontDescent(size float64, bold bool) float64 {
	m := face(size, bold).Metrics()
	return float64(m.Descent) / 64 / renderScale
}

// centerBaseline 在高 h 的容器（顶 y）内垂直居中后的文字 baseline。
// 必须用 ascent+descent 全高居中，只按 ascent 算会让中文视觉偏上。
func centerBaseline(y, h, size float64, bold bool) float64 {
	m := face(size, bold).Metrics()
	asc, desc := float64(m.Ascent)/64/renderScale, float64(m.Descent)/64/renderScale
	return y + (h-asc-desc)/2 + asc
}

// drawTextCentered 在顶 y、高 h 的容器里从水平起点 x 垂直居中绘制文字。
func drawTextCentered(dc *gg.Context, text string, x, y, h, size float64, hex string, weight int) {
	if text == "" {
		return
	}
	baseline := centerBaseline(y, h, size, isBold(weight))
	setText(dc, size, hex, weight)
	drawGlyph(dc, text, x, baseline)
}

func lineHeight(size float64) float64 { return math.Round(size * 1.42) }

// ---------------- 颜色工具 ----------------

func hexRGB(hex string) (int, int, int) {
	hex = strings.TrimPrefix(hex, "#")
	var r, g, b int
	fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b)
	return r, g, b
}

func col(hex string, alpha float64) color.Color {
	r, g, b := hexRGB(hex)
	return color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: uint8(alpha * 255)}
}

// lerpHex 在两个颜色间按 t 插值（渐变背景用）。
func lerpHex(a, b string, t float64) color.Color {
	ar, ag, ab := hexRGB(a)
	br, bg, bb := hexRGB(b)
	return color.NRGBA{
		R: uint8(float64(ar) + (float64(br)-float64(ar))*t),
		G: uint8(float64(ag) + (float64(bg)-float64(ag))*t),
		B: uint8(float64(ab) + (float64(bb)-float64(ab))*t),
		A: 255,
	}
}

// ---------------- 文字工具 ----------------

func setText(dc *gg.Context, size float64, hex string, weight int) {
	dc.SetFontFace(face(size, isBold(weight)))
	dc.SetColor(col(hex, 1))
}

func drawText(dc *gg.Context, text string, x, yTop, size float64, hex string, weight int) {
	if text == "" {
		return
	}
	setText(dc, size, hex, weight)
	baseline := yTop + fontAscent(size, isBold(weight))
	drawGlyph(dc, text, x, baseline)
}

// measureTextW 测宽；文字按粗体绘制时传 weight 以获得精确宽度。返回逻辑宽度。
func measureTextW(text string, size float64, weight ...int) float64 {
	w := 400
	if len(weight) > 0 {
		w = weight[0]
	}
	dc := measureCtx()
	dc.SetFontFace(face(size, isBold(w)))
	wt, _ := dc.MeasureString(text)
	return wt / renderScale
}

// truncateToWidth 超宽时二分加省略号（对齐 JS truncateToWidth）。
func truncateToWidth(text string, maxWidth, size float64, weight ...int) string {
	w := 400
	if len(weight) > 0 {
		w = weight[0]
	}
	text = cleanText(text)
	if text == "" || measureTextW(text, size, w) <= maxWidth {
		return text
	}
	chars := []rune(text)
	lo, hi := 0, len(chars)
	best := "…"
	for lo <= hi {
		mid := (lo + hi) / 2
		candidate := strings.TrimRight(string(chars[:mid]), " ") + "…"
		if measureTextW(candidate, size, w) <= maxWidth {
			best = candidate
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// wrapText 逐字换行，超出 maxLines 的最后一行截断加省略号。
func wrapText(text string, maxWidth, size float64, maxLines int, weight ...int) []string {
	w := 400
	if len(weight) > 0 {
		w = weight[0]
	}
	chars := []rune(cleanText(text))
	if len(chars) == 0 {
		return nil
	}
	var lines []string
	line := strings.Builder{}
	for _, ch := range chars {
		candidate := line.String() + string(ch)
		if line.Len() == 0 || measureTextW(candidate, size, w) <= maxWidth {
			line.WriteString(string(ch))
			continue
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
		line.Reset()
		line.WriteString(strings.TrimLeft(string(ch), " "))
	}
	if line.Len() > 0 {
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	if len(lines) <= maxLines {
		return lines
	}
	visible := lines[:maxLines]
	visible[maxLines-1] = truncateToWidth(visible[maxLines-1]+"…", maxWidth, size, w)
	return visible
}

func cleanText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// stripHTML 见下；StripMarkdown 处理 JM 简介里混进的 Markdown 强调标记。
//
// 接口返回的作品简介/评论里会出现 "**加粗**" 这类标记，直接画出来会带着星号。
func StripMarkdown(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	return strings.TrimSpace(s)
}

// htmlTagRe 匹配任意 HTML 标签。
var htmlTagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// htmlBlockRe 匹配应当被替换成空格的块级/换行标签，
// 否则 "<div>a</div><div>b</div>" 会被拼成 "ab" 而不是 "a b"。
var htmlBlockRe = regexp.MustCompile(`(?i)</?(br|p|div|li|tr|h[1-6]|blockquote)\b[^>]*>`)

// 评论/简介接口返回的 content 是 HTML 片段，例如：
//
//	<div style='flex-direction:row;flex-wrap:wrap;'>百合什么的最好了😋</div>
//
// 原实现只做了空白折叠，于是这些标签会被原样画进图片里。这里在排版前把标签
// 去掉、把实体还原，再交给 cleanText 统一折叠空白。
func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	s = htmlBlockRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func formatNumber(v int) string {
	switch {
	case v >= 100000000:
		return trimFixed(float64(v)/100000000) + "亿"
	case v >= 10000:
		return trimFixed(float64(v)/10000) + "万"
	default:
		return fmt.Sprint(v)
	}
}

func trimFixed(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}

// ---------------- 通用绘制 ----------------

func roundedRectPath(dc *gg.Context, x, y, w, h, r float64) {
	dc.DrawRoundedRectangle(x, y, w, h, r)
}

// softShadow 生成一个高斯模糊的暗色圆角矩形投影图层（设备分辨率，paste 贴回）。
// 返回图比 (w,h) 四周各多出 60 逻辑 px 的出血，供模糊扩散；调用方用 paste(dc, img, -60, -60) 贴。
func softShadow(w, h, radius, dy, sigma float64, alpha uint8) *image.NRGBA {
	s := renderScale
	dc := gg.NewContext(int((w+120)*s), int((h+120)*s))
	dc.DrawRoundedRectangle(60*s, (60+dy)*s, w*s, h*s, radius*s)
	dc.SetColor(color.NRGBA{R: 0x10, G: 0x18, B: 0x28, A: alpha})
	dc.Fill()
	return imaging.Blur(dc.Image(), sigma*s)
}

// verticalGradientRows 在 clip 内逐行画垂直渐变（步长 1 逻辑 px，避免放大后色带）。
func verticalGradientRows(dc *gg.Context, x, y, w, h float64, top, bottom string) {
	for row := 0; row < int(h); row++ {
		t := float64(row) / h
		dc.SetColor(lerpHex(top, bottom, t))
		dc.DrawRectangle(x, y+float64(row), w, 1)
		dc.Fill()
	}
}

// clippedImage 把 src 以 cover 模式填满 w×h（设备分辨率），并按矢量路径裁出圆角。
func clippedImage(src image.Image, w, h, radius float64) image.Image {
	filled := imaging.Fill(src, int(math.Round(w*renderScale)), int(math.Round(h*renderScale)), imaging.Center, imaging.Lanczos)
	dc := newCanvas(w, h)
	roundedRectPath(dc, 0, 0, w, h, radius)
	dc.Clip()
	paste(dc, filled, 0, 0)
	return dc.Image()
}

// roundedImage 从文件加载图片并裁成圆角方块（勋章用）。
func roundedImage(path string, size, radius float64) (image.Image, error) {
	src, err := imaging.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open badge %q: %w", path, err)
	}
	return clippedImage(src, size, size, radius), nil
}

// pill 胶囊标签，返回宽度。
func pill(dc *gg.Context, text string, x, y, h, size, padX, maxWidth float64, fg, bg, stroke string, weight int) float64 {
	safe := truncateToWidth(text, maxWidth-padX*2, size, weight)
	textW := measureTextW(safe, size, weight)
	w := math.Min(maxWidth, math.Max(h, textW+padX*2))
	roundedRectPath(dc, x, y, w, h, h/2)
	dc.SetColor(col(bg, 1))
	dc.Fill()
	if stroke != "" {
		dc.SetLineWidth(1)
		dc.SetColor(col(stroke, 1))
		roundedRectPath(dc, x+0.5, y+0.5, w-1, h-1, h/2)
		dc.Stroke()
	}
	tw := measureTextW(safe, size, weight)
	setText(dc, size, fg, weight)
	drawGlyph(dc, safe, x+(w-tw)/2, centerBaseline(y, h, size, isBold(weight)))
	return w
}

// ---------------- 图标（简单形状近似） ----------------

func heartIcon(dc *gg.Context, x, y, s float64, colorHex string) {
	dc.SetColor(col(colorHex, 0.14))
	heartPath(dc, x, y, s)
	dc.Fill()
	dc.SetLineWidth(2.2)
	dc.SetColor(col(colorHex, 1))
	heartPath(dc, x, y, s)
	dc.Stroke()
}

func heartPath(dc *gg.Context, x, y, s float64) {
	dc.MoveTo(x+0.5*s, y+0.88*s)
	dc.CubicTo(x-0.06*s, y+0.5*s, x+0.14*s, y+0.12*s, x+0.5*s, y+0.34*s)
	dc.CubicTo(x+0.86*s, y+0.12*s, x+1.06*s, y+0.5*s, x+0.5*s, y+0.88*s)
	dc.ClosePath()
}

func commentIcon(dc *gg.Context, x, y, s float64, colorHex string) {
	dc.SetColor(col(colorHex, 0.12))
	dc.DrawRoundedRectangle(x, y+0.08*s, 0.92*s, 0.68*s, 0.2*s)
	dc.Fill()
	dc.SetLineWidth(2.2)
	dc.SetColor(col(colorHex, 1))
	dc.DrawRoundedRectangle(x, y+0.08*s, 0.92*s, 0.68*s, 0.2*s)
	dc.Stroke()
	dc.MoveTo(x+0.18*s, y+0.74*s)
	dc.LineTo(x+0.18*s, y+0.96*s)
	dc.LineTo(x+0.42*s, y+0.74*s)
	dc.ClosePath()
	dc.SetColor(col(colorHex, 0.12))
	dc.Fill()
}

func eyeIcon(dc *gg.Context, x, y, s float64, colorHex string) {
	dc.SetLineWidth(2.2)
	dc.SetColor(col(colorHex, 1))
	dc.DrawEllipse(x+0.5*s, y+0.5*s, 0.5*s, 0.33*s)
	dc.Stroke()
	dc.SetColor(col(colorHex, 0.25))
	dc.DrawCircle(x+0.5*s, y+0.5*s, 0.14*s)
	dc.Fill()
	dc.SetColor(col(colorHex, 1))
	dc.DrawCircle(x+0.5*s, y+0.5*s, 0.14*s)
	dc.SetLineWidth(2.2)
	dc.Stroke()
}

// ---------------- 相册卡片（对应 renderAlbum.js） ----------------

const (
	cardW, cardH, cardR = 1080.0, 640.0, 34.0
	coverX, coverY      = 52.0, 58.0
	coverW, coverH      = 330.0, 524.0
	coverR              = 26.0
	contentX            = 428.0
	contentW            = 600.0
)

type album struct {
	id, name, desc      string
	like, comment, view int
	tags                []string
	author              []string
	addTime             string
	series              int
	chapters            []string // 章节名，按顺序；为空则不画章节行
	reading             int      // 正在看的章节号（1-based，0=无选中），章节区高亮用
	coverPath           string   // 为空则用占位图
}

func renderAlbumCard(a album, outputPath string) error {
	renderMu.Lock()
	defer renderMu.Unlock()

	// 1) 封面与模糊背景
	var coverSrc image.Image
	if a.coverPath != "" {
		img, err := imaging.Open(a.coverPath)
		if err != nil {
			return fmt.Errorf("open cover %q: %w", a.coverPath, err)
		}
		coverSrc = img
	} else {
		coverSrc = placeholderCover(a.name)
	}
	blurBg := imaging.AdjustBrightness(imaging.AdjustSaturation(
		imaging.Blur(imaging.Fill(coverSrc, int(cardW*renderScale), int(cardH*renderScale), imaging.Center, imaging.Lanczos), 34*renderScale),
		0.65), 1.08)

	// 2) 画布
	dc := newCanvas(cardW, cardH)
	paste(dc, blurBg, 0, 0)
	// 白色蒙版
	dc.SetColor(col("#ffffff", 0.84))
	roundedRectPath(dc, 0, 0, cardW, cardH, cardR)
	dc.Fill()
	// 投影 + 内层玻璃卡
	paste(dc, softShadow(cardW-56, cardH-56, cardR, 18, 12, 30), -60, -60)
	dc.SetColor(col("#ffffff", 0.87))
	roundedRectPath(dc, 28, 28, cardW-56, cardH-56, cardR)
	dc.Fill()
	dc.SetLineWidth(1)
	dc.SetColor(col("#ffffff", 0.86))
	roundedRectPath(dc, 28.5, 28.5, cardW-57, cardH-57, cardR)
	dc.Stroke()

	// 3) 圆角封面 + 白描边
	paste(dc, clippedImage(coverSrc, coverW, coverH, coverR), coverX, coverY)
	dc.SetLineWidth(1)
	dc.SetColor(col("#ffffff", 0.86))
	roundedRectPath(dc, coverX+0.5, coverY+0.5, coverW-1, coverH-1, coverR)
	dc.Stroke()

	// 4) 分隔线
	dc.SetColor(col("#eaecf0", 1))
	dc.DrawLine(410, 72, 410, 568)
	dc.SetLineWidth(1)
	dc.Stroke()

	// 5) 右侧内容
	addAlbumDetails(dc, a)

	return dc.SavePNG(outputPath)
}

func placeholderCover(name string) image.Image {
	dc := newCanvas(900, 1200)
	verticalGradientRows(dc, 0, 0, 900, 1200, "#eef2ff", "#e0f2fe")
	dc.SetColor(col("#dbeafe", 0.75))
	dc.DrawCircle(735, 175, 210)
	dc.Fill()
	dc.SetColor(col("#ede9fe", 0.78))
	dc.DrawCircle(110, 1020, 250)
	dc.Fill()
	dc.SetColor(col("#ffffff", 0.72))
	dc.DrawRoundedRectangle(118, 420, 664, 360, 52)
	dc.Fill()
	initials := []rune(cleanText(name))
	if len(initials) > 4 {
		initials = initials[:4]
	}
	text := string(initials)
	size := 84.0
	w := measureTextW(text, size, 700)
	setText(dc, size, "#475467", 700)
	drawGlyph(dc, text, 450-w/2, 610)
	return dc.Image()
}

func addAlbumDetails(dc *gg.Context, a album) {
	y := coverY
	// ID chip
	w := pill(dc, "ID "+a.id, contentX, y, 30, 17, 13, 170, "#4f46e5", "#eef2ff", "#c7d2fe", 500)
	_ = w
	y += 46

	// 标题（最多 2 行）
	for _, line := range wrapText(a.name, contentW, 38, 2, 700) {
		drawText(dc, line, contentX, y, 38, "#182230", 700)
		y += 47
	}
	y += 12

	// 简介（最多 4 行，空间不足自动收缩；多预留一点给章节行）
	descLines := wrapText(a.desc, contentW, 21, 4)
	for descMax := 4; descMax >= 2; descMax-- {
		if y+float64(len(descLines))*30+240 <= coverY+524 {
			break
		}
		descLines = wrapText(a.desc, contentW, 21, descMax-1)
	}
	for _, line := range descLines {
		drawText(dc, line, contentX, y, 21, "#667085", 400)
		y += 30
	}
	y += 20

	// 统计行
	y += addStats(dc, a, contentX, y)
	y += 22

	// 标签
	y += addTags(dc, a.tags, contentX, y, contentW)
	if y+24 > coverY+524-68 {
		y = coverY + 524 - 68
	} else {
		y += 24
	}
	addMeta(dc, a, contentX, y)

	// 章节胶囊区：meta 行下方，向下逐行铺到内卡底边
	if len(a.chapters) > 0 {
		chY := y + 2*32 + 12
		maxBottom := 28 + (cardH - 56) - 16 // 内卡底边留 16
		if chY > maxBottom-30 {
			chY = maxBottom - 30
		}
		addChapters(dc, a.chapters, contentX, chY, contentW, maxBottom-chY, a.reading)
	}
}

// addChapters 章节胶囊区：从左到右逐行铺满可用空间，放不下时中间插省略号胶囊、
// 保住"正在看"的章节和最后一章。reading 为正在看的章节号（1-based，0 表示无选中），
// 选中胶囊用实心高亮样式；若它落在被省略的区间里，会紧跟省略号单独钉出来。
// 返回实际占用高度。容量不靠估算：先逐个量宽，再模拟流式布局找最大可容纳前缀。
func addChapters(dc *gg.Context, chapters []string, x, y, maxW, maxH float64, reading int) float64 {
	const (
		gap, rowGap, h, size, padX = 8.0, 10.0, 30.0, 15.0, 12.0
		pillMax                    = 200.0
		fg, bg, stroke             = "#4f46e5", "#eef2ff", "#c7d2fe"
		selFg, selBg, selStroke    = "#ffffff", "#4f46e5", "#4338ca"
	)
	last := len(chapters) - 1
	if last < 0 {
		return 0
	}
	maxRows := int((maxH + rowGap) / (h + rowGap))
	if maxRows < 1 {
		maxRows = 1
	}

	width := func(text string, weight int) float64 {
		return math.Min(pillMax, math.Max(h, measureTextW(truncateToWidth(text, pillMax-padX*2, size, weight), size, weight)+padX*2))
	}
	ellsW := width("…", 700)

	selIdx := -1
	if reading >= 1 && reading <= len(chapters) {
		selIdx = reading - 1
	}
	ws := make([]float64, len(chapters))
	for i, c := range chapters {
		wt := 500
		if i == selIdx {
			wt = 700
		}
		ws[i] = width(c, wt)
	}

	// 收尾必须露出的胶囊：最后一章；选中的章节若不在前缀里，也钉在收尾区。
	suffix := func(front int) []int {
		if selIdx >= 0 && selIdx != last && front <= selIdx {
			return []int{selIdx, last}
		}
		return []int{last}
	}
	// -1 表示省略号胶囊。
	buildItems := func(front int) []int {
		items := make([]int, 0, front+4)
		for i := 0; i < front; i++ {
			items = append(items, i)
		}
		if front >= len(chapters) {
			return items
		}
		suf := suffix(front)
		if front < suf[0] {
			items = append(items, -1)
		}
		for k, si := range suf {
			// 收尾项之间跳档（如选中章和末章不相邻）也要补省略号，不然跳得太突兀
			if k > 0 && si > suf[k-1]+1 {
				items = append(items, -1)
			}
			items = append(items, si)
		}
		return items
	}
	// flowFits 模拟流式布局：items 依序排进最多 maxRows 行、每行 maxW，是否装得下。
	flowFits := func(items []int) bool {
		rowW, rows := 0.0, 1
		for _, i := range items {
			w := ellsW
			if i >= 0 {
				w = ws[i]
			}
			need := w
			if rowW > 0 {
				need += gap
			}
			if rowW+need > maxW {
				rows++
				if rows > maxRows {
					return false
				}
				rowW = w
			} else {
				rowW += need
			}
		}
		return true
	}

	bestFront := -1
	for front := 0; front <= len(chapters); front++ {
		if !flowFits(buildItems(front)) {
			break
		}
		bestFront = front
	}
	items := buildItems(bestFront)
	if bestFront < 0 { // 理论兜底：收尾区都装不下时只露省略号+末章
		items = []int{-1, last}
	}

	drawPill := func(i int, px, py float64) float64 {
		switch {
		case i < 0:
			return pill(dc, "…", px, py, h, size, padX, pillMax, fg, bg, stroke, 700)
		case i == selIdx:
			return pill(dc, chapters[i], px, py, h, size, padX, pillMax, selFg, selBg, selStroke, 700)
		default:
			return pill(dc, chapters[i], px, py, h, size, padX, pillMax, fg, bg, stroke, 500)
		}
	}
	curX, curY := x, y
	for _, i := range items {
		w := ellsW
		if i >= 0 {
			w = ws[i]
		}
		if curX > x && curX-x+w > maxW {
			curX = x
			curY += h + rowGap
		}
		curX += drawPill(i, curX, curY) + gap
	}
	return curY + h - y
}

func addStats(dc *gg.Context, a album, x, y float64) float64 {
	items := []struct {
		icon func(*gg.Context, float64, float64, float64, string)
		hex  string
		text string
	}{
		{heartIcon, "#e11d48", formatNumber(a.like)},
		{commentIcon, "#2563eb", formatNumber(a.comment)},
		{eyeIcon, "#059669", formatNumber(a.view)},
	}
	cur := x
	for _, it := range items {
		setText(dc, 20, "#344054", 500)
		lw := measureTextW(it.text, 20)
		paste(dc, iconLayer(it.icon, it.hex, 22), cur, y+6)
		drawText(dc, it.text, cur+22+8, y, 20, "#344054", 500)
		cur += 22 + 8 + lw + 30
	}
	return 34
}

// iconLayer 把矢量图标画到透明小画布再贴过去（设备分辨率）。
func iconLayer(draw func(*gg.Context, float64, float64, float64, string), hex string, size int) image.Image {
	dc := newCanvas(float64(size), float64(size))
	draw(dc, 0, 0, float64(size), hex)
	return dc.Image()
}

func addTags(dc *gg.Context, tags []string, x, y, maxWidth float64) float64 {
	if len(tags) == 0 {
		tags = []string{"暂无标签"}
	}
	const (
		gap, rowGap, h, size, maxRows = 10.0, 10.0, 32.0, 18.0, 2
		pillMax                       = 168.0
	)
	// 先量宽，再逐行放置
	widths := make([]float64, len(tags))
	hidden := 0
	for i, t := range tags {
		widths[i] = math.Min(pillMax, math.Max(h, measureTextW(truncateToWidth(t, pillMax-28, size), size)+28))
	}
	// 装进最多 maxRows 行，装不下的折算成 +N
	var rows [][]int
	rowW := -1.0 // -1 表示还没有行
	for i := range tags {
		need := widths[i]
		newRow := rowW >= 0 && rowW+gap+need > maxWidth
		if newRow && len(rows) >= maxRows {
			hidden = len(tags) - i
			break
		}
		if rowW < 0 || newRow {
			rows = append(rows, nil)
			rowW = 0
		} else {
			need = gap + widths[i]
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], i)
		rowW += need
	}

	curY := y
	for _, row := range rows {
		curX := x
		for _, idx := range row {
			pill(dc, tags[idx], curX, curY, h, size, 14, pillMax, "#344054", "#f2f4f7", "#d0d5dd", 400)
			curX += widths[idx] + gap
		}
		curY += h + rowGap
	}
	if hidden > 0 {
		curX := x
		if len(rows) > 0 {
			for _, idx := range rows[len(rows)-1] {
				curX += widths[idx] + gap
			}
		}
		pill(dc, fmt.Sprintf("+%d", hidden), curX, y+float64(len(rows)-1)*(h+rowGap), h, size, 14, 72, "#667085", "#eef2ff", "#c7d2fe", 500)
	}
	rowsH := float64(len(rows))*h + math.Max(0, float64(len(rows)-1))*rowGap
	return rowsH
}

func addMeta(dc *gg.Context, a album, x, y float64) {
	labelSize, valueSize, labelW, rowH := 17.0, 20.0, 72.0, 32.0

	author := "未知作者"
	if len(a.author) > 0 {
		author = strings.Join(a.author, " / ")
	}
	row := func(label, value string, rowY float64) {
		drawText(dc, label, x, rowY+2, labelSize, "#98a2b3", 500)
		drawText(dc, truncateToWidth(value, contentW-labelW, valueSize), x+labelW, rowY, valueSize, "#344054", 500)
	}
	row("作者", author, y)
	row("发布", a.addTime, y+rowH)

	// 章节 pill
	boxW, boxH := 138.0, 34.0
	chipX := x + contentW - boxW
	chipY := y + rowH - 2
	roundedRectPath(dc, chipX, chipY, boxW, boxH, 17)
	dc.SetColor(col("#eef2ff", 1))
	dc.Fill()
	dc.SetLineWidth(1)
	dc.SetColor(col("#c7d2fe", 1))
	roundedRectPath(dc, chipX+0.5, chipY+0.5, boxW-1, boxH-1, 17)
	dc.Stroke()
	drawTextCentered(dc, "章节", chipX+16, chipY, boxH, 17, "#98a2b3", 500)
	series := fmt.Sprintf("%d 章", a.series)
	sw := measureTextW(series, 20, 700)
	drawTextCentered(dc, series, chipX+boxW-sw-16, chipY, boxH, 20, "#4f46e5", 700)
}

// ---------------- 评论区（对应 renderComment.js） ----------------

const pageW = 960.0

type commentData struct {
	nickname, content, addTime, uid string
	likes                           int
	level                           int
	levelName                       string
	avatarPath                      string   // 真实头像图片；为空用首字占位
	badges                          []string // 勋章图片路径，跟在昵称/等级后
	// inline 正文里的表情图：图片直链 → 本地文件路径。
	inline  map[string]string
	replies []commentData
}

// inlineImageStore 缓存本次渲染里反复出现的表情图。
//
// 同一张贴纸常被一堆人用（一页评论里重复十几次很常见），同一张图在不同字号
// （顶级评论 25px / 楼中楼 22px）下又要各自缩一次，所以键带上目标边长。
type inlineImageStore struct {
	items map[string]image.Image
}

func newInlineImageStore() *inlineImageStore {
	return &inlineImageStore{items: map[string]image.Image{}}
}

// load 打开并缩放到边长不超过 size 设备像素的位图（保持长宽比）。
func (s *inlineImageStore) load(path string, size int) (image.Image, error) {
	key := path + "|" + strconv.Itoa(size)
	if img, ok := s.items[key]; ok {
		return img, nil
	}
	if size <= 0 {
		return nil, fmt.Errorf("表情图缩放尺寸非法: %d", size)
	}
	src, err := imaging.Open(path)
	if err != nil {
		return nil, err
	}
	img := imaging.Fit(src, size, size, imaging.Lanczos)
	if img.Bounds().Dx() == 0 || img.Bounds().Dy() == 0 {
		return nil, fmt.Errorf("表情图缩放后为空")
	}
	s.items[key] = img
	return img, nil
}

func renderCommentPage(title, note string, comments []commentData, outputPath string) error {
	renderMu.Lock()
	defer renderMu.Unlock()

	const (
		padX, padY      = 34.0, 34.0
		headerH, gap    = 62.0, 24.0
		footerH, panelR = 18.0, 28.0
	)
	contentW := pageW - padX*2

	// 1) 先算总高（块图为设备分辨率，换算回逻辑高度）
	store := newInlineImageStore()
	blocks := make([]image.Image, 0, len(comments))
	heights := make([]float64, 0, len(comments))
	total := padY + headerH + footerH
	for i := range comments {
		blk := renderCommentBlock(comments[i], contentW, 0, 3, store)
		blocks = append(blocks, blk)
		h := float64(blk.Bounds().Dy()) / renderScale
		heights = append(heights, h)
		total += h + gap
	}
	pageH := total

	// 2) 画布
	dc := newCanvas(pageW, pageH)
	verticalGradientRows(dc, 0, 0, pageW, pageH, "#f8fafc", "#eef2ff")
	paste(dc, softShadow(pageW-44, pageH-44, panelR-10, 18, 12, 25), -60, -60)
	dc.SetColor(col("#ffffff", 0.62))
	roundedRectPath(dc, 22, 22, pageW-44, pageH-44, panelR-10)
	dc.Fill()

	// 3) 头部
	drawText(dc, title, padX, padY+4, 32, "#202939", 700)
	nw := measureTextW(note, 18, 600)
	drawText(dc, note, pageW-padX-nw, padY+10, 18, "#4f46e5", 600)

	// 4) 评论块
	y := padY + headerH
	for i, blk := range blocks {
		paste(dc, blk, padX, y)
		y += heights[i] + gap
	}

	return dc.SavePNG(outputPath)
}

// renderCommentBlock 渲染单条评论（含缩进回复），返回裁剪好的块图。
func renderCommentBlock(c commentData, width float64, depth, maxReplies int, store *inlineImageStore) image.Image {
	compact := depth > 0
	avatarSize, avatarLeft, avatarGap := 58.0, 18.0, 20.0
	nameSize, chipH := 22.0, 26.0
	textSize, padBX, padBY, bubbleR := 25.0, 20.0, 14.0, 18.0
	metaSize, heartSize := 17.0, 20.0
	topPad, bottomPad, gapHeader, metaGap := 14.0, 14.0, 10.0, 10.0
	bubbleShrink := 36.0
	bg, panelLine := "#ffffff", "#eaecf0"

	if compact {
		avatarSize, avatarLeft, avatarGap = 44, 72, 16
		nameSize, chipH = 19, 24
		textSize, padBX, padBY, bubbleR = 22, 18, 12, 16
		metaSize, heartSize = 16, 18
		topPad, bottomPad, gapHeader, metaGap = 10, 8, 8, 8
		bubbleShrink = 18
		bg = "#f8fafc"
	}

	bodyX := avatarLeft + avatarSize + avatarGap
	bodyW := width - bodyX
	if !compact {
		bodyW -= 18
	}
	bubbleMaxW := bodyW - bubbleShrink
	maxTextW := bubbleMaxW - padBX*2

	// 布局测量
	badgeSize, badgeGap := 24.0, 6.0
	if compact {
		badgeSize, badgeGap = 21, 6
	}
	badgesW := float64(len(c.badges)) * (badgeSize + badgeGap)
	name := truncateToWidth(c.nickname, math.Max(120, bodyW-14-110-badgesW), nameSize)
	nameW := measureTextW(name, nameSize)
	nameH := lineHeight(nameSize) * 0.75
	// 评论正文是 HTML 片段，里面混着文字和两种表情图（服务端贴纸 <img>、
	// Unicode 码位映射的 Twemoji）。排版前必须先把标签摘掉，否则标签会被
	// 当成正文画进图里；但**贴纸要留下来当图排**，不能一并抹掉。
	emojiPx := int(math.Round(inlineEmojiSize(textSize) * renderScale))
	lines := layoutInline(c.content, maxTextW, textSize, 400, 1000, func(url, alt string) image.Image {
		path := c.inline[url]
		if path == "" {
			return nil
		}
		img, err := store.load(path, emojiPx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "表情图跳过:", err)
			return nil
		}
		return img
	})
	// 兜底只在「真的没有任何可画内容」时触发：正文为空，或者只剩没 src 的标签。
	// 老实现按 stripHTML 后的空串判空，于是纯表情评论（正文就是一串 <img>）
	// 全被误判成「这条评论没有内容」。
	if len(lines) == 0 {
		const empty = "这条评论没有内容"
		w := measureTextW(empty, textSize, 400)
		lines = []inlineLine{{segs: []inlineSeg{{text: empty, w: w}}, w: w}}
	}
	lineH := lineHeight(textSize)
	paraH := float64(len(lines)) * lineH
	bubbleW := math.Min(bubbleMaxW, math.Max(148, maxInlineW(lines)+padBX*2))
	bubbleH := paraH + padBY*2
	metaText := c.addTime
	if !compact {
		metaText = c.addTime + "   UID " + c.uid
	}
	metaH := math.Max(lineHeight(metaSize)*0.75, heartSize)

	// 回复
	shown := c.replies
	if maxReplies >= 0 && len(shown) > maxReplies {
		shown = shown[:maxReplies]
	} else if maxReplies < 0 {
		shown = nil
	}
	var replyImgs []image.Image
	var repliesH float64
	for _, r := range shown {
		ri := renderCommentBlock(r, width, depth+1, -1, store)
		replyImgs = append(replyImgs, ri)
		repliesH += float64(ri.Bounds().Dy()) / renderScale
	}
	var moreH float64
	hidden := len(c.replies) - len(shown)
	moreText := ""
	if hidden > 0 {
		moreText = fmt.Sprintf("还有 %d 条回复", hidden)
		moreH = lineHeight(17) + 12
	}

	blockH := topPad + nameH + gapHeader + bubbleH + metaGap + metaH + bottomPad + repliesH + moreH
	if compact {
		blockH -= bottomPad - 8
	}

	// 画块
	dc := newCanvas(width, blockH+8)
	// 顶级评论白底圆角面板
	if !compact {
		roundedRectPath(dc, 0, 0, width, blockH, 24)
		dc.SetColor(col(bg, 1))
		dc.Fill()
		dc.SetLineWidth(1)
		dc.SetColor(col(panelLine, 1))
		roundedRectPath(dc, 0.5, 0.5, width-1, blockH-1, 24)
		dc.Stroke()
	}
	// 头像
	paste(dc, avatarImage(c.nickname, avatarSize, c.avatarPath), avatarLeft, topPad)
	// 连接线
	if compact {
		dc.SetLineWidth(2)
		dc.SetColor(col("#d9dee8", 1))
		dc.DrawLine(35, 0, 35, blockH-12)
		dc.Stroke()
		dc.DrawLine(35, topPad+avatarSize/2, avatarLeft-12, topPad+avatarSize/2)
		dc.Stroke()
	} else if len(replyImgs) > 0 {
		dc.SetLineWidth(2)
		dc.SetColor(col("#d9dee8", 1))
		dc.DrawLine(avatarLeft+avatarSize/2, topPad+avatarSize+10, avatarLeft+avatarSize/2, blockH-22)
		dc.Stroke()
	}

	// 昵称 + 等级徽章 + 勋章图片：三者统一以 nameH 带子中心为基准线。
	// 名字必须按行框全高（ascent+descent）在带内居中——drawText 会把 baseline
	// 直接放在 topPad+ascent，而 Noto 的 hhea ascent≈1.16em 远高于汉字字形顶
	// ≈0.88em，中文墨迹会整体下沉、一半掉到带子外面。
	drawTextCentered(dc, name, bodyX, topPad, nameH, nameSize, "#202939", 600)
	chipText := fmt.Sprintf("LV%d", c.level)
	if c.levelName != "" {
		chipText += " " + c.levelName
	}
	chipText = truncateToWidth(chipText, 172, 17)
	chipW := math.Max(40, math.Min(190, measureTextW(chipText, 17, 600)+18))
	chipX := bodyX + nameW + 10
	chipY := topPad + (nameH-chipH)/2
	roundedRectPath(dc, chipX, chipY, chipW, chipH, chipH/2)
	dc.SetColor(col("#ede9fe", 1))
	dc.Fill()
	bx := chipX + (chipW-measureTextW(chipText, 17, 600))/2
	setText(dc, 17, "#7c3aed", 600)
	drawGlyph(dc, chipText, bx, centerBaseline(chipY, chipH, 17, true))

	// 勋章图片：等级后依次排列，圆角方形（对齐模板 .badge 24/21px, radius 7）
	curX := chipX + chipW + badgeGap
	for _, bp := range c.badges {
		bi, err := roundedImage(bp, badgeSize, 7)
		if err != nil {
			fmt.Fprintln(os.Stderr, "勋章跳过:", err)
			continue
		}
		paste(dc, bi, curX, topPad+(nameH-badgeSize)/2)
		curX += badgeSize + badgeGap
	}

	// 气泡
	bubbleY := topPad + nameH + gapHeader
	bubbleLine := "#d0d5dd" // 气泡边框比面板描边(#eaecf0)深一档，太浅会显得没轮廓
	roundedRectPath(dc, bodyX, bubbleY, bubbleW, bubbleH, bubbleR)
	dc.SetColor(col(bg, 1))
	dc.Fill()
	dc.SetLineWidth(1)
	dc.SetColor(col(bubbleLine, 1))
	roundedRectPath(dc, bodyX+0.5, bubbleY+0.5, bubbleW-1, bubbleH-1, bubbleR)
	dc.Stroke()
	for i, ln := range lines {
		drawInlineLine(dc, ln, bodyX+padBX, bubbleY+padBY+float64(i)*lineH, textSize, "#202939", 400)
	}

	// 元信息 + 点赞：时间文字 / 爱心 / 数字三者共用 metaH 中线，
	// 都按行框全高居中（老的 0.75 倍行高带子会让文字墨迹比爱心低约 0.27em）。
	metaY := bubbleY + bubbleH + metaGap
	drawTextCentered(dc, metaText, bodyX, metaY, metaH, metaSize, "#98a2b3", 500)
	likeText := fmt.Sprint(c.likes)
	lw := measureTextW(likeText, metaSize)
	metaW := measureTextW(metaText, metaSize)
	heartX := math.Min(bodyX+bubbleW-heartSize-6-lw, bodyX+bodyW-heartSize-6-lw)
	heartX = math.Max(heartX, bodyX+metaW+18) // 不能压到时间文字上（对齐 JS 的保护逻辑）
	heartIcon(dc, heartX, metaY+(metaH-heartSize)/2, heartSize, "#e11d48")
	drawTextCentered(dc, likeText, heartX+heartSize+6, metaY, metaH, metaSize, "#98a2b3", 400)

	// 回复
	replyY := metaY + metaH + bottomPad
	for _, ri := range replyImgs {
		paste(dc, ri, 0, replyY)
		replyY += float64(ri.Bounds().Dy()) / renderScale
	}
	if moreText != "" {
		drawText(dc, moreText, bodyX, replyY+2, 17, "#4f46e5", 600)
	}

	return imaging.Crop(dc.Image(), image.Rect(0, 0, int(math.Round(width*renderScale)), int(math.Round(blockH*renderScale))))
}

// avatarImage 头像：有真实图片走圆形裁剪，否则渐变+首字占位。
func avatarImage(name string, size float64, imgPath string) image.Image {
	if imgPath != "" {
		if src, err := imaging.Open(imgPath); err == nil {
			return clippedImage(src, size, size, size/2)
		}
	}
	dc := newCanvas(size, size)
	verticalGradientRows(dc, 0, 0, size, size, "#e0f2fe", "#ede9fe")
	label := "?"
	if r := []rune(cleanText(name)); len(r) > 0 {
		label = string(r[0])
	}
	fs := math.Round(size * 0.42)
	w := measureTextW(label, fs, 700)
	setText(dc, fs, "#475467", 700)
	drawGlyph(dc, label, size/2-w/2, centerBaseline(0, size, fs, true))
	// 圆形裁剪
	masked := newCanvas(size, size)
	masked.DrawCircle(size/2, size/2, size/2)
	masked.Clip()
	paste(masked, dc.Image(), 0, 0)
	return masked.Image()
}

// ---------------- 对外入口 ----------------
//
// 下面的类型是对内网结构（album / commentData）的公开替身：内部字段是小写的，
// 包外没法直接构造。转换只做字段搬运，绘制逻辑一律复用上面的实现。

// AlbumCard 是渲染「作品详情卡片」的入参，字段与 renderAlbum.js 的输入对齐。
type AlbumCard struct {
	ID      string
	Name    string
	Desc    string
	Like    int
	Comment int
	View    int
	Tags    []string
	Author  []string
	AddTime string
	Series  int
	// Chapters 章节名，按顺序；为空则不画章节行。
	Chapters []string
	// Reading 正在阅读的章节号（1-based，0 表示无选中），用于高亮。
	Reading int
	// CoverPath 封面本地路径；为空则用首字占位封面。
	CoverPath string
}

// RenderAlbumCard 渲染作品详情卡片并写出 PNG。
func RenderAlbumCard(card AlbumCard, outputPath string) error {
	regular, boldPath := fontPaths()
	if !FontsReady() {
		return fmt.Errorf("渲染详情卡片: 字体不可用 (regular=%q bold=%q)，请先配置 fonts 或用 config_set 指定", regular, boldPath)
	}
	if outputPath == "" {
		return fmt.Errorf("渲染详情卡片: 输出路径为空")
	}
	if err := EnsureDir(filepath.Dir(outputPath)); err != nil {
		return err
	}
	return renderAlbumCard(album{
		id:        card.ID,
		name:      card.Name,
		desc:      card.Desc,
		like:      card.Like,
		comment:   card.Comment,
		view:      card.View,
		tags:      card.Tags,
		author:    card.Author,
		addTime:   card.AddTime,
		series:    card.Series,
		chapters:  card.Chapters,
		reading:   card.Reading,
		coverPath: card.CoverPath,
	}, outputPath)
}

// Comment 是渲染「评论区长图」的入参。
type Comment struct {
	Nickname string
	// Content 允许直接传接口返回的 HTML 片段，渲染前会自动剥离标签；
	// 里面的表情图（<img> 贴纸与 Unicode 码位映射的 Twemoji）会被排进气泡，
	// 具体见 InlineImages。
	Content   string
	AddTime   string
	UID       string
	Likes     int
	Level     int
	LevelName string
	// AvatarPath 头像本地路径；为空则用昵称首字占位。
	AvatarPath string
	// Badges 勋章图片本地路径，跟在等级徽章后面。
	Badges []string
	// InlineImages 正文表情图的本地缓存：图片直链 → 本地文件路径。
	//
	// 渲染层不做网络请求，图必须先由调用方（service）预抓落盘。
	// 键就是 utils.CollectInlineImages 给出的 URL，缺项的表情会退化成
	// [alt] / 原始码位文字，不会因此整条评论变成"没有内容"。
	InlineImages map[string]string
	// Replies 楼中楼回复。
	Replies []Comment
}

// RenderCommentPage 渲染评论区图片并写出 PNG。
func RenderCommentPage(title, note string, comments []Comment, outputPath string) error {
	regular, boldPath := fontPaths()
	if !FontsReady() {
		return fmt.Errorf("渲染评论图: 字体不可用 (regular=%q bold=%q)，请先配置 fonts 或用 config_set 指定", regular, boldPath)
	}
	if outputPath == "" {
		return fmt.Errorf("渲染评论图: 输出路径为空")
	}
	if err := EnsureDir(filepath.Dir(outputPath)); err != nil {
		return err
	}

	items := make([]commentData, len(comments))
	for i, c := range comments {
		items[i] = toCommentData(c)
	}
	return renderCommentPage(title, note, items, outputPath)
}

func toCommentData(c Comment) commentData {
	replies := make([]commentData, len(c.Replies))
	for i, r := range c.Replies {
		replies[i] = toCommentData(r)
	}
	return commentData{
		nickname:   c.Nickname,
		content:    c.Content,
		addTime:    c.AddTime,
		uid:        c.UID,
		likes:      c.Likes,
		level:      c.Level,
		levelName:  c.LevelName,
		avatarPath: c.AvatarPath,
		badges:     c.Badges,
		inline:     c.InlineImages,
		replies:    replies,
	}
}

// PlaceholderImage 生成一张占位图：白底 + 浅色描边 + 居中说明文字。
//
// 用途是 PDF 合成时替换读不出来的图片——直接跳过会让后续所有页码错位。
// 字体不可用时退化成纯白底（不报错），保证 PDF 流程不被字体配置拖死。
func PlaceholderImage(width, height int, text string) image.Image {
	if width <= 0 {
		width = 1000
	}
	if height <= 0 {
		height = 1414
	}
	// 占位图不需要高分辨率，限制一下，避免超长图占用大量内存。
	const maxSide = 4096
	if width > maxSide || height > maxSide {
		scale := math.Min(float64(maxSide)/float64(width), float64(maxSide)/float64(height))
		width = int(float64(width) * scale)
		height = int(float64(height) * scale)
	}

	dc := gg.NewContext(width, height)
	dc.SetColor(color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})
	dc.Clear()
	dc.SetColor(color.NRGBA{R: 0xE4, G: 0xE7, B: 0xEC, A: 0xFF})
	dc.SetLineWidth(4)
	dc.DrawRectangle(16, 16, float64(width)-32, float64(height)-32)
	dc.Stroke()

	// 中央画一个叉
	dc.SetColor(color.NRGBA{R: 0xE1, G: 0x1D, B: 0x48, A: 0x66})
	dc.SetLineWidth(8)
	cx, cy := float64(width)/2, float64(height)/2
	r := math.Min(float64(width), float64(height)) * 0.08
	dc.DrawLine(cx-r, cy-r, cx+r, cy+r)
	dc.DrawLine(cx-r, cy+r, cx+r, cy-r)
	dc.Stroke()

	if !FontsReady() || text == "" {
		return dc.Image()
	}

	size := math.Max(22, math.Min(48, float64(width)/22))
	placeholderFace := face(size, false)
	dc.SetFontFace(placeholderFace)
	dc.SetColor(color.NRGBA{R: 0x66, G: 0x70, B: 0x85, A: 0xFF})

	lines := wrapText(text, float64(width)-96, size, 3)
	lineH := lineHeight(size)
	startY := cy + r + lineH*2
	for i, line := range lines {
		w, _ := dc.MeasureString(line)
		dc.DrawString(line, (float64(width)-w)/2, startY+float64(i)*lineH)
	}
	return dc.Image()
}

// WritePlaceholder 生成一张占位图并写出到 path。
//
// 用途：下载/还原失败时**必须**在对应位置留一张图，否则 PDF 的页数会少于
// 真实章节页数，读者看到的页码会从缺口之后整体前移一位——这种错位比
// 一张写着「第 5 页不可用」的占位图糟糕得多。
//
// 输出格式由扩展名决定（.png / .jpg / .jpeg），其余扩展名按 PNG 处理。
func WritePlaceholder(path string, width, height int, text string) error {
	if path == "" {
		return fmt.Errorf("写出占位图: 路径为空")
	}
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}

	img := PlaceholderImage(width, height, text)

	// 占位图同样走「先写 .part 再改名」：它会被当成该页的正经产物喂进 PDF，
	// 半截文件比"缺页"更难发现（页数还是对的，只是那一页是花的）。
	//
	// 原来这里有个 jpg/其它两分支的 switch，但两支的代码完全一样，已去掉；
	// 代价是 encoding 不再由 imaging.Save 按扩展名推断，要显式做一次。
	format, err := imaging.FormatFromFilename(path)
	if err != nil {
		return fmt.Errorf("写出占位图: 无法从 %q 推断输出格式: %w", path, err)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("写出占位图: 创建 %q 失败: %w", tmp, err)
	}
	if err := imaging.Encode(f, img, format); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("写出占位图: 写出 %q 失败: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("写出占位图: 关闭 %q 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("写出占位图: 落盘 %q 失败: %w", path, err)
	}
	return nil
}
