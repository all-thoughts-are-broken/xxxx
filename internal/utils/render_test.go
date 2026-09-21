package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disintegration/imaging"
)

// withTestFonts 把包里默认字体临时指向仓库内的 fonts/ 目录，测完还原。
//
// 用 t.Cleanup 还原而不是直接 SetupFonts 一拍走人：字体是包级变量，
// 泄漏出去会影响同包里别的测试（比如占位图在字体可用时会多画一段文字）。
func withTestFonts(t *testing.T) {
	t.Helper()

	oldReg, oldBold := FontPaths()
	t.Cleanup(func() { SetupFonts(oldReg, oldBold) })

	root := filepath.Join("..", "..", "fonts")
	SetupFonts(
		filepath.Join(root, "NotoSansSC-Regular.ttf"),
		filepath.Join(root, "Noto-Sans-SC-Bold-2.ttf"),
	)
	if !FontsReady() {
		t.Skipf("仓库内字体不可用（%s），跳过渲染冒烟测试", root)
	}
}

// ---------------- 文字清洗 ----------------

func TestCleanTextCollapsesWhitespace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  a   b  ", "a b"},
		{"a\n\nb\tc", "a b c"},
		{"", ""},
		{"   ", ""},
		{"单行", "单行"},
		// 全角空格(U+3000)在 unicode.IsSpace 里，所以会被折叠成半角空格。
		// 这对中文排版是好事（JM 的简介里两种空格混用），写在这里防止
		// 有人把 Fields 换成 Split 之后没人发现。
		{"a\u3000b", "a b"},
		{"中文\u3000\u3000缩进", "中文 缩进"},
	}
	for _, c := range cases {
		if got := cleanText(c.in); got != c.want {
			t.Errorf("cleanText(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestStripMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**加粗**", "加粗"},
		{"有一天，我发现了一本能实现任何禁忌愿望的笔记本。\r\n\r\n**", "有一天，我发现了一本能实现任何禁忌愿望的笔记本。"},
		{"__下划线__", "下划线"},
		{"没有标记", "没有标记"},
		{"", ""},
	}
	for _, c := range cases {
		if got := StripMarkdown(c.in); got != c.want {
			t.Errorf("StripMarkdown(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestStripHTML 覆盖评论正文的真实形态。
//
// 接口返回的 content 是 HTML 片段，原实现只折叠空白，标签会被原样画进图里。
func TestStripHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		// 真实抓到的形态
		{
			`<div style='flex-direction:row;flex-wrap:wrap;'>百合什么的最好了😋</div>`,
			"百合什么的最好了😋",
		},
		// 块级标签之间要有空格，否则 ab 会被粘成一个词
		{"<div>a</div><div>b</div>", "a b"},
		{"a<br>b", "a b"},
		{"<p>第一段</p><p>第二段</p>", "第一段 第二段"},
		// 实体还原
		{"a&amp;b", "a&b"},
		{"&lt;不是标签&gt;", "<不是标签>"},
		// 嵌套
		{`<div><span style="color:red">红</span>字</div>`, "红 字"},
		// 干净文本走快路径，原样返回
		{"普通评论", "普通评论"},
		{"", ""},
		// 带 & 但不成实体，UnescapeString 会原样保留
		{"a & b", "a & b"},
	}
	for _, c := range cases {
		if got := stripHTML(c.in); got != c.want {
			t.Errorf("stripHTML(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestStripHTMLEscapesBeforeTagRemoval 确认处理顺序不会把用户写的
// "&lt;script&gt;" 变成真标签再被删掉。
func TestStripHTMLEscapesBeforeTagRemoval(t *testing.T) {
	got := stripHTML("&lt;script&gt;alert(1)&lt;/script&gt;")
	if !strings.Contains(got, "<script>") {
		t.Errorf("转义后的标签应当原样还原为文本，实际 = %q", got)
	}
}

// ---------------- 颜色工具 ----------------

func TestHexRGB(t *testing.T) {
	cases := []struct {
		in      string
		r, g, b int
	}{
		{"#ffffff", 255, 255, 255},
		{"#000000", 0, 0, 0},
		{"e11d48", 0xE1, 0x1D, 0x48},
		{"#f8fafc", 0xF8, 0xFA, 0xFC},
	}
	for _, c := range cases {
		r, g, b := hexRGB(c.in)
		if r != c.r || g != c.g || b != c.b {
			t.Errorf("hexRGB(%q) = (%d,%d,%d), 期望 (%d,%d,%d)", c.in, r, g, b, c.r, c.g, c.b)
		}
	}
}

func TestLerpHexEndpoints(t *testing.T) {
	// t=0 取 a，t=1 取 b
	lo := lerpHex("#000000", "#ffffff", 0)
	r, g, b, _ := lo.RGBA()
	if r != 0 || g != 0 || b != 0 {
		t.Errorf("lerpHex t=0 应当是起点色，实际 (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
	hi := lerpHex("#000000", "#ffffff", 1)
	r, g, b, _ = hi.RGBA()
	if r>>8 != 255 || g>>8 != 255 || b>>8 != 255 {
		t.Errorf("lerpHex t=1 应当是终点色，实际 (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

func TestLineHeightScalesWithSize(t *testing.T) {
	if lineHeight(16) >= lineHeight(20) {
		t.Errorf("字号越大行高应当越大: %v vs %v", lineHeight(16), lineHeight(20))
	}
	// 行高必须大于字号，否则中文上下会粘连
	if lineHeight(20) <= 20 {
		t.Errorf("行高 %.0f 不大于字号 20，中文会粘连", lineHeight(20))
	}
}

// ---------------- 渲染冒烟（需要字体） ----------------

// TestRenderAlbumCardDimensions 确认详情卡片的成图尺寸。
//
// 1080x640 逻辑尺寸 × renderScale(2) = 2160x1280，与 rendercard 原型一致。
func TestRenderAlbumCardDimensions(t *testing.T) {
	withTestFonts(t)

	out := filepath.Join(t.TempDir(), "card.png")
	// CoverPath 留空 → 走 placeholderCover 分支，不需要外部素材
	err := RenderAlbumCard(AlbumCard{
		ID: "1472136", Name: "禁忌笔记",
		Desc: "有一天，我发现了一本能实现任何禁忌愿望的笔记本。",
		Like: 2722, Comment: 25, View: 36235,
		Tags:     []string{"韩漫", "机翻", "后宫"},
		Author:   []string{"N/A"},
		AddTime:  "2026-01-01 00:00:00",
		Series:   5,
		Chapters: []string{"第1章", "第2章", "第3章", "第4章", "第5章"},
		Reading:  2,
	}, out)
	if err != nil {
		t.Fatalf("渲染详情卡片失败: %v", err)
	}

	assertImageSize(t, out, 2160, 1280)
}

// TestRenderAlbumCardMinimalInput 确认极简输入（无章节/无作者/无标签/无简介）
// 不会崩，也不会退化出零尺寸画布。
func TestRenderAlbumCardMinimalInput(t *testing.T) {
	withTestFonts(t)

	out := filepath.Join(t.TempDir(), "minimal.png")
	if err := RenderAlbumCard(AlbumCard{ID: "1", Name: "只有名字"}, out); err != nil {
		t.Fatalf("极简输入渲染失败: %v", err)
	}
	assertImageSize(t, out, 2160, 1280)
}

// TestRenderAlbumCardBadCoverPathErrors 确认显式给了封面路径但读不出来时报错。
//
// 这里是"报错"而不是"静默换占位图"：路径是调用方给的，读不到说明调用方
// 的判断出了问题（比如下载没成功却仍然传了路径），静默兜底会掩盖这类 bug。
// 想要占位封面就把 CoverPath 留空。
func TestRenderAlbumCardBadCoverPathErrors(t *testing.T) {
	withTestFonts(t)

	err := RenderAlbumCard(AlbumCard{
		ID:        "1",
		Name:      "封面路径不存在",
		CoverPath: filepath.Join(t.TempDir(), "nope.jpg"),
	}, filepath.Join(t.TempDir(), "x.png"))
	if err == nil {
		t.Errorf("封面读不出来时应当报错（留空 CoverPath 才会用占位封面）")
	}
}

// TestRenderCommentPageDimensions 确认评论区图宽度固定、高度随内容增长。
func TestRenderCommentPageDimensions(t *testing.T) {
	withTestFonts(t)

	dir := t.TempDir()
	one := filepath.Join(dir, "one.png")
	if err := RenderCommentPage("评论区", "禁忌笔记 · AID 1472136", []Comment{
		{Nickname: "潜水员", Content: "感谢分享！", AddTime: "2026-09-03 22:05", UID: "100865", Likes: 1, Level: 1},
	}, one); err != nil {
		t.Fatalf("渲染评论图失败: %v", err)
	}
	assertImageSize(t, one, 1920, 0) // 只校验宽度

	many := filepath.Join(dir, "many.png")
	if err := RenderCommentPage("评论区", "note", []Comment{
		{Nickname: "甲", Content: "第一条", Level: 12, LevelName: "传说"},
		{Nickname: "乙", Content: "第二条"},
		{Nickname: "丙", Content: "第三条"},
	}, many); err != nil {
		t.Fatalf("渲染多条评论失败: %v", err)
	}

	oneImg, err := imaging.Open(one)
	if err != nil {
		t.Fatal(err)
	}
	manyImg, err := imaging.Open(many)
	if err != nil {
		t.Fatal(err)
	}
	if manyImg.Bounds().Dy() <= oneImg.Bounds().Dy() {
		t.Errorf("三条评论的图应当比一条高: %d vs %d",
			manyImg.Bounds().Dy(), oneImg.Bounds().Dy())
	}
}

// TestRenderCommentPageHandlesHTMLContent 确认评论正文里的 HTML 标签
// 不会被当成正文画进图里（宽度不变、不报错）。
func TestRenderCommentPageHandlesHTMLContent(t *testing.T) {
	withTestFonts(t)

	out := filepath.Join(t.TempDir(), "html.png")
	err := RenderCommentPage("评论区", "note", []Comment{
		{
			Nickname: "百合",
			Content:  `<div style='flex-direction:row;flex-wrap:wrap;'>百合什么的最好了😋</div>`,
		},
	}, out)
	if err != nil {
		t.Fatalf("渲染带 HTML 的评论失败: %v", err)
	}
	assertImageSize(t, out, 1920, 0)
}

// TestRenderRejectsEmptyOutputPath 确认空输出路径给出明确错误，
// 而不是安静地画完然后丢掉结果。
func TestRenderRejectsEmptyOutputPath(t *testing.T) {
	withTestFonts(t)

	if err := RenderAlbumCard(AlbumCard{Name: "x"}, ""); err == nil {
		t.Errorf("空输出路径应当报错（详情卡片）")
	}
	if err := RenderCommentPage("评论区", "", nil, ""); err == nil {
		t.Errorf("空输出路径应当报错（评论图）")
	}
}

// ---------------- 占位图 ----------------

func TestWritePlaceholderWritesDecodableImage(t *testing.T) {
	withTestFonts(t)

	dir := t.TempDir()
	for _, name := range []string{"p.png", "p.jpg", "p.jpeg"} {
		p := filepath.Join(dir, name)
		if err := WritePlaceholder(p, 300, 420, "图片读取失败: 00007.jpg"); err != nil {
			t.Fatalf("写出占位图 %s 失败: %v", name, err)
		}
		assertImageSize(t, p, 300, 420)
	}
}

func TestWritePlaceholderCreatesParentDir(t *testing.T) {
	withTestFonts(t)

	p := filepath.Join(t.TempDir(), "deep", "nested", "p.png")
	if err := WritePlaceholder(p, 100, 100, "x"); err != nil {
		t.Fatalf("应当自动创建父目录: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("占位图没有落盘: %v", err)
	}
}

func TestWritePlaceholderRejectsEmptyPath(t *testing.T) {
	if err := WritePlaceholder("", 100, 100, "x"); err == nil {
		t.Errorf("空路径应当报错")
	}
}

// TestPlaceholderImageClampsHugeSize 确认超大尺寸被限制。
//
// 占位图是按原图尺寸生成的，而漫画长条图可能有几万像素高；
// 不限制的话一张占位图就能吃掉几百 MB 内存。
func TestPlaceholderImageClampsHugeSize(t *testing.T) {
	// 先确保字体状态不影响这条测试的尺寸断言
	img := PlaceholderImage(50000, 90000, "x")
	b := img.Bounds()
	if b.Dx() > 4096 || b.Dy() > 4096 {
		t.Errorf("超大尺寸没有被限制: %dx%d", b.Dx(), b.Dy())
	}
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Errorf("尺寸退化: %dx%d", b.Dx(), b.Dy())
	}
	// 宽高比应当保持
	ratioIn := 50000.0 / 90000.0
	ratioOut := float64(b.Dx()) / float64(b.Dy())
	if diff := ratioIn - ratioOut; diff > 0.02 || diff < -0.02 {
		t.Errorf("缩放后宽高比失真: 输入 %.4f, 输出 %.4f", ratioIn, ratioOut)
	}
}

func TestPlaceholderImageFallbackSize(t *testing.T) {
	// 宽高传 0 时应当退回到 A4 比例，而不是生成一张 0x0 的图
	img := PlaceholderImage(0, 0, "x")
	b := img.Bounds()
	if b.Dx() != 1000 || b.Dy() != 1414 {
		t.Errorf("零尺寸应当退回 1000x1414，实际 %dx%d", b.Dx(), b.Dy())
	}
}

// ---------------- 测试小工具 ----------------

// assertImageSize 打开图片校验尺寸；wantH 传 0 表示只校验宽度。
func assertImageSize(t *testing.T, path string, wantW, wantH int) {
	t.Helper()

	img, err := imaging.Open(path)
	if err != nil {
		t.Fatalf("产出图打不开 (%s): %v", path, err)
	}
	b := img.Bounds()
	if b.Dx() != wantW {
		t.Errorf("%s 宽度 = %d, 期望 %d", filepath.Base(path), b.Dx(), wantW)
	}
	if wantH > 0 && b.Dy() != wantH {
		t.Errorf("%s 高度 = %d, 期望 %d", filepath.Base(path), b.Dy(), wantH)
	}
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Errorf("%s 尺寸退化: %dx%d", filepath.Base(path), b.Dx(), b.Dy())
	}
}
