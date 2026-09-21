package utils

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disintegration/imaging"
)

// ---------------- emoji → Twemoji 文件名 ----------------

// TestEmojiKeyMatchesTwemojiFileNames 锁定 emoji 码位 → Twemoji 文件名的映射。
//
// 期望值不是从参考实现抄的，是**对着 cdnjs 上的 14.0.2 目录实测**的：
//   - `2764.png` 存在、`2764-fe0f.png` 是 404 → 变体选择符必须去掉
//   - `1f44d-1f3fb.png`、`1f468-200d-1f469-200d-1f467.png`、`1f1e8-1f1f3.png` 存在
//   - `0031-20e3.png` 是 404（Twemoji 不补前导零）
//
// native 版参考实现把 FE0F 编进了 key，于是 ❤️ 这类**基本都会 404**
// 掉进 □ 兜底 —— 这条测试就是为了别把那个 bug 抄过来。
func TestEmojiKeyMatchesTwemojiFileNames(t *testing.T) {
	// 一律写成转义序列，不写字面量：ZWJ(200d) 与变体选择符是不可见字符，
	// 任何一次复制粘贴/编辑器保存都可能把它们吃掉，然后测试就以
	// "看起来一样、码位不一样" 的方式骗过所有人（写这份测试时已经踩过一次，
	// 字面量 👨👩👧 里的两个 ZWJ 就是这么没的）。
	cases := []struct {
		text     string
		wantKey  string
		wantUsed int
	}{
		{"\U0001F60B", "1f60b", 1},                 // 😋
		{"\U0001F603", "1f603", 1},                 // 😃
		{"\u2B50", "2b50", 1},                      // ⭐
		{"\u2764\ufe0f", "2764", 2},                // ❤️：FE0F 只消耗、不进 key
		{"\U0001F44D\U0001F3FD", "1f44d-1f3fd", 2}, // 👍🏽：肤色修饰符保留
		{"\U0001F1E8\U0001F1F3", "1f1e8-1f1f3", 2}, // 🇨🇳：区域指示符成对
		{"\U0001F468\u200d\U0001F469\u200d\U0001F467", // 👨👩👧：ZWJ 序列整体保留
			"1f468-200d-1f469-200d-1f467", 5},
	}
	for _, c := range cases {
		gotKey, gotUsed := emojiKey([]rune(c.text))
		if gotKey != c.wantKey || gotUsed != c.wantUsed {
			t.Errorf("emojiKey(%q) = (%q, %d)，期望 (%q, %d)",
				c.text, gotKey, gotUsed, c.wantKey, c.wantUsed)
		}
	}
}

// TestEmojiKeyAlwaysAdvances 锁死「至少消费一个码位」。
//
// 这是防死循环的硬约束：emojiKey 在修饰符循环里 break 掉之后由调用方推进，
// 一旦哪天返回 used=0，textRuns 就会卡死在第一个 emoji 上（表现为渲染线程挂住）。
func TestEmojiKeyAlwaysAdvances(t *testing.T) {
	for _, s := range []string{"", "a", "😋", "\u200d", "\ufe0f", "\U0001f3fb"} {
		if _, used := emojiKey([]rune(s)); used < 1 {
			t.Errorf("emojiKey(%q) 消费了 %d 个码位，必须 >= 1", s, used)
		}
	}
}

// ---------------- 分词 ----------------

func TestTokenizeInlineSeparatesStickersEmojiAndText(t *testing.T) {
	content := `<div style='flex-direction:row;flex-wrap:wrap;'>前面` +
		`<img src='https://www.cdnbea.net/media/emoji/aa.png' alt="惊喜" />` +
		`中间😋后面</div>`

	runs := tokenizeInline(content)
	got := make([]string, 0, len(runs))
	for _, r := range runs {
		switch r.kind {
		case inlineTextRun:
			got = append(got, "t:"+r.text)
		case inlineStickerRun:
			got = append(got, "s:"+r.url+"|"+r.alt)
		case inlineEmojiRun:
			got = append(got, "e:"+r.key)
		}
	}
	want := []string{
		"t:前面",
		"s:https://www.cdnbea.net/media/emoji/aa.png|惊喜",
		"t:中间",
		"e:1f60b",
		"t:后面",
	}
	if strings.Join(got, " / ") != strings.Join(want, " / ") {
		t.Errorf("分词结果 = %v，期望 %v", got, want)
	}
}

// TestTokenizeInlineDropsTagsWithoutSrc 确认没有 src 的 <img> 不会留下空 run
// （留下会让纯标签正文看起来"有内容"，把渲染层的空正文兜底给绕过去）。
func TestTokenizeInlineDropsTagsWithoutSrc(t *testing.T) {
	runs := tokenizeInline(`<div><img alt="x"></div>`)
	if len(runs) != 0 {
		t.Errorf("无 src 的 <img> 应当被丢掉，实际得到 %d 个 run: %+v", len(runs), runs)
	}
}

// TestTokenizeInlineIgnoresDataSrc 确认只有真正的 `src=` 才算图片地址。
//
// `data-src="..."` 是惰性加载的常见写法，而 `-` 与 `s` 之间正好是词边界：
// 用 `\bsrc` 匹配会把它当成 src 抓走一个错地址（轻则 404，重则把别的图
// 当表情画上去）。
func TestTokenizeInlineIgnoresDataSrc(t *testing.T) {
	runs := tokenizeInline(`<div><img data-src="https://x/lazy.png" src="https://x/real.png"></div>`)
	if len(runs) != 1 {
		t.Fatalf("应当只切出 1 个 run，实际 %d 个: %+v", len(runs), runs)
	}
	if runs[0].url != "https://x/real.png" {
		t.Errorf("取到的地址 = %q，期望 https://x/real.png（data-src 不算数）", runs[0].url)
	}
}

func TestCollectInlineImagesDedupesAndMarksStickers(t *testing.T) {
	sticker := "https://www.cdnbea.net/media/emoji/aa.png"
	content := `<div><img src="` + sticker + `" alt="惊喜" />` +
		`<img src="` + sticker + `" alt="惊喜" />` + // 同一张出现两次
		`<img src='` + sticker + `' /></div>😋`

	got := CollectInlineImages(content)
	if len(got) != 2 {
		t.Fatalf("去重后应当只剩 2 条（1 张贴纸 + 1 个 Twemoji），实际 %d: %+v", len(got), got)
	}
	if !got[0].Sticker || got[0].URL != sticker || got[0].Alt != "惊喜" {
		t.Errorf("第 0 条 = %+v，期望贴纸 %s", got[0], sticker)
	}
	if got[1].Sticker || got[1].URL != TwemojiURL("1f60b") {
		t.Errorf("第 1 条 = %+v，期望 Twemoji %s", got[1], TwemojiURL("1f60b"))
	}
}

// ---------------- 混排布局 ----------------

// solidImage 造一张纯色方形位图（模拟已缩放到设备分辨率的表情图）。
func solidImage(px int, c color.Color) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, px, px))
	for y := 0; y < px; y++ {
		for x := 0; x < px; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// TestLayoutInlineEmojiOnlyContentIsNotEmpty 是本轮 bug 的回归测试。
//
// 只发表情（正文就是一串 <img>）的评论过去被 stripHTML 洗成空串，落进
// 「这条评论没有内容」兜底分支。现在它必须排出一行图，且**一个文字段都没有**。
func TestLayoutInlineEmojiOnlyContentIsNotEmpty(t *testing.T) {
	withTestFonts(t)

	content := `<div style='flex-direction:row;flex-wrap:wrap;'>` +
		`<img style="width:18px;height:18px" src="https://www.cdnbea.net/media/emoji/a.png" alt="惊喜" />` +
		`<img style="width:18px;height:18px" src="https://www.cdnbea.net/media/emoji/b.png" alt="生病" />` +
		`</div>`
	img := solidImage(48, color.NRGBA{R: 255, G: 0, B: 255, A: 255})

	lines := layoutInline(content, 400, 25, 400, 1000, func(string, string) image.Image { return img })
	if len(lines) != 1 {
		t.Fatalf("两张表情应当排在一行，实际 %d 行: %v", len(lines), inlineLinesText(lines))
	}
	for i, seg := range lines[0].segs {
		if seg.img == nil {
			t.Errorf("第 %d 段是文字 %q，纯表情评论不该有文字段", i, seg.text)
		}
	}
	if want := 2 * (48.0 / renderScale); lines[0].w != want {
		t.Errorf("行宽 = %v，期望 %v（两个表情宽度之和）", lines[0].w, want)
	}
}

func TestLayoutInlineWrapsWithinMaxWidth(t *testing.T) {
	withTestFonts(t)

	const maxW = 300.0
	img := solidImage(56, color.NRGBA{R: 255, G: 0, B: 255, A: 255})
	content := strings.Repeat("这是一句很长的中文评论，", 5) + "😋" +
		strings.Repeat("继续写点什么好呢", 8)

	lines := layoutInline(content, maxW, 25, 400, 1000, func(string, string) image.Image { return img })
	if len(lines) < 2 {
		t.Fatalf("这么长的正文应当折成多行，实际 %d 行", len(lines))
	}
	for i, ln := range lines {
		if ln.w > maxW {
			t.Errorf("第 %d 行宽 %v 超过 %v: %v", i, ln.w, maxW, inlineLinesText(lines)[i])
		}
		var sum float64
		for _, seg := range ln.segs {
			sum += seg.w
		}
		if sum != ln.w {
			t.Errorf("第 %d 行的段宽之和 %v != 行宽 %v（测宽与绘制会错位）", i, sum, ln.w)
		}
	}
}

// TestLayoutInlineTruncatesToMaxLines 确认长正文按 maxLines 截断且补省略号。
func TestLayoutInlineTruncatesToMaxLines(t *testing.T) {
	withTestFonts(t)

	lines := layoutInline(strings.Repeat("很长很长的一段话", 30), 200, 25, 400, 3, nil)
	if len(lines) != 3 {
		t.Fatalf("行数 = %d，期望 3", len(lines))
	}
	last := lines[2]
	if got := inlineLinesText(lines)[2]; !strings.HasSuffix(got, "…") {
		t.Errorf("末行 = %q，期望以省略号结尾", got)
	}
	// 补上的省略号必须计入行宽：气泡宽度是按行宽算的，漏算会让最后一行的
	// 文字溢出气泡（或者反过来留一大截空白）。
	var sum float64
	for _, seg := range last.segs {
		sum += seg.w
	}
	if sum != last.w {
		t.Errorf("末行段宽之和 %v != 行宽 %v（省略号没计入行宽）", sum, last.w)
	}
}

// TestLayoutInlineFallsBackToText 确认图拿不到时退回文字，而不是把这段内容丢掉。
//
// 丢掉是最坏的选择：全是表情的评论会因此又变成「没有内容」。
func TestLayoutInlineFallsBackToText(t *testing.T) {
	withTestFonts(t)

	lines := layoutInline(`<div><img src="https://x/media/emoji/a.png" alt="惊喜"></div>`, 400, 25, 400, 1000, nil)
	if got := strings.Join(inlineLinesText(lines), ""); got != "[惊喜]" {
		t.Errorf("贴纸取不到图时的兜底 = %q，期望 %q", got, "[惊喜]")
	}

	// 没有 alt 的贴纸也得留下点东西。
	lines = layoutInline(`<div><img src="https://x/media/emoji/a.png"></div>`, 400, 25, 400, 1000, nil)
	if got := strings.Join(inlineLinesText(lines), ""); got != "[表情]" {
		t.Errorf("无 alt 贴纸的兜底 = %q，期望 %q", got, "[表情]")
	}

	// Unicode emoji 取不到 Twemoji 时退回原始码位（主字体画 □，与参考实现一致）。
	lines = layoutInline("😋", 400, 25, 400, 1000, nil)
	if got := strings.Join(inlineLinesText(lines), ""); got != "😋" {
		t.Errorf("emoji 取不到图时的兜底 = %q，期望 %q", got, "😋")
	}
}

// TestLayoutInlineEmptyContent 确认真的空正文才返回空（渲染层据此画「没有内容」）。
func TestLayoutInlineEmptyContent(t *testing.T) {
	withTestFonts(t)

	for _, s := range []string{"", "   ", "<div></div>", "<img>"} {
		if lines := layoutInline(s, 400, 25, 400, 1000, nil); len(lines) != 0 {
			t.Errorf("layoutInline(%q) 返回了 %d 行，期望 0 行", s, len(lines))
		}
	}
}

// ---------------- 渲染层：表情真的落到图上 ----------------

// TestRenderCommentPageDrawsStickerImages 端到端确认贴纸被画进评论图。
//
// 用一个调色板里没有的颜色（洋红）当贴纸，渲染完直接数洋红像素：
// 调色板全是蓝/灰/白/玫红，洋红只能来自贴纸本身，所以这个断言不会"假绿"。
func TestRenderCommentPageDrawsStickerImages(t *testing.T) {
	withTestFonts(t)

	dir := t.TempDir()
	stickerPath := filepath.Join(dir, "sticker.png")
	writeSolidPNG(t, stickerPath, 48, color.NRGBA{R: 255, G: 0, B: 255, A: 255})
	const url = "https://www.cdnbea.net/media/emoji/deadbeef.png"
	content := `<div style='flex-direction:row;flex-wrap:wrap;'><img src="` + url + `" alt="惊喜" /></div>`

	with := filepath.Join(dir, "with.png")
	if err := RenderCommentPage("评论区", "note", []Comment{{
		Nickname: "羽", Level: 10, LevelName: "无际的青空",
		Content: content, InlineImages: map[string]string{url: stickerPath},
	}}, with); err != nil {
		t.Fatalf("渲染带表情的评论失败: %v", err)
	}
	if n := countMagentaPixels(t, with); n == 0 {
		t.Errorf("表情图没被画进评论图（洋红像素为 0）—— 纯表情评论又变回空白了")
	}

	// 对照：本地图缺失时不能凭空画出贴纸，但仍要能正常出图（走 [惊喜] 兜底）。
	without := filepath.Join(dir, "without.png")
	if err := RenderCommentPage("评论区", "note", []Comment{{
		Nickname: "羽", Content: content,
	}}, without); err != nil {
		t.Fatalf("渲染缺表情图的评论失败: %v", err)
	}
	if n := countMagentaPixels(t, without); n != 0 {
		t.Errorf("图没准备好却画出了 %d 个洋红像素", n)
	}
}

func writeSolidPNG(t *testing.T, path string, px int, c color.Color) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, solidImage(px, c)); err != nil {
		t.Fatal(err)
	}
}

// countMagentaPixels 数洋红像素（R>200 且 B>200 且 G<80）。
func countMagentaPixels(t *testing.T, path string) int {
	t.Helper()
	img, err := imaging.Open(path)
	if err != nil {
		t.Fatalf("打开渲染结果失败: %v", err)
	}
	b := img.Bounds()
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 > 200 && bl>>8 > 200 && g>>8 < 80 {
				n++
			}
		}
	}
	return n
}
