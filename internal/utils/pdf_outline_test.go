package utils

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"

	"testing"
	"unicode/utf16"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// ---- 书签文本编码 ----
//
// 这一组测试锁的是一个**看起来纯属多余、删掉会立刻坏**的行为：
// 写书签前必须把标题转成 UTF-16BE + BOM。
//
// 背景（2026-09-21 实测）：
//   - gofpdf 只在「当前字体是 UTF-8 字体」时才转换书签标题。本项目的 PDF
//     页面上是纯图片、不加载字体，isCurrentUTF8 恒为 false，于是它把 Go
//     字符串按原始字节写进 /Title。
//   - PDF 规范规定：不带 BOM 的字符串按 PDFDocEncoding 解释。"第" 的
//     UTF-8 字节 E7 AC AC 落在 PDFDocEncoding 里就是 "ç¬¬"，
//     真实阅读器（实测 WPS）整棵大纲树全是乱码。

func TestOutlineTextEncodesUTF16BE(t *testing.T) {
	cases := []struct {
		in   string
		want []byte
	}{
		{"", nil},
		{
			"第1章",
			[]byte{0xFE, 0xFF, 0x7B, 0x2C, 0x00, 0x31, 0x7A, 0xE0},
		},
		{
			"12", // 纯 ASCII 也要带 BOM：PDFDocEncoding 是 byte 表，
			// 混排中英的书签只有统一走 UTF-16 才不会出现半截乱码。
			[]byte{0xFE, 0xFF, 0x00, 0x31, 0x00, 0x32},
		},
		{
			// 补充平面字符（U+1F600）必须拆成代理对，否则阅读器显示空白。
			"\U0001F600",
			[]byte{0xFE, 0xFF, 0xD8, 0x3D, 0xDE, 0x00},
		},
	}

	for _, c := range cases {
		got := []byte(outlineText(c.in))
		if !bytes.Equal(got, c.want) {
			t.Errorf("outlineText(%q) = % x, 期望 % x", c.in, got, c.want)
		}
		if c.in != "" {
			if !bytes.HasPrefix(got, []byte{0xFE, 0xFF}) {
				t.Errorf("outlineText(%q) 缺少 UTF-16BE BOM: % x", c.in, got)
			}
		}
	}
}

// TestOutlineTextRoundTrips 确认编码可逆（含代理对）。
func TestOutlineTextRoundTrips(t *testing.T) {
	for _, s := range []string{"第1章", "封面", "第123話 - 序章", "emoji \U0001F600 混排", "12"} {
		raw := []byte(outlineText(s))

		if !bytes.HasPrefix(raw, []byte{0xFE, 0xFF}) {
			t.Fatalf("%q: 没有 BOM", s)
		}
		units := make([]uint16, 0, (len(raw)-2)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
		}
		if got := string(utf16.Decode(units)); got != s {
			t.Errorf("往返后 = %q, 期望 %q", got, s)
		}
	}
}

// titleLiteralRe 抓 PDF 里的 /Title ( ... ) 字面串。
//
// 故意做得很松：只要括号内不含未转义的右括号即可 —— 书签标题里出现
// "(" 的概率极低，而写一个完整的 PDF 字符串解析器超出了测试的需要。
var titleLiteralRe = regexp.MustCompile(`/Title\s*\(([^)]*)\)`)

// TestPdfBookmarkTitlesAreUTF16InRawBytes 是**真正能抓住乱码 bug** 的断言。
//
// 为什么不能只靠「读回来等于 "第1章"」：
// pdfcpu 的 StringLiteralToString 里写着
//
//	// If no acceptable UTF16 encoding is found, accept real-world UTF8
//	// before falling back to PDFDocEncoding.
//
// 也就是说 pdfcpu 会**宽容地把无 BOM 的裸 UTF-8 也当 UTF-8 解**。所以
// 「pdfcpu 读回来是对的」既可能是我们写对了，也可能是我们写错了但 pdfcpu
// 帮我们补了 —— 旧版测试正是在这里假绿，真实阅读器照样满屏乱码。
//
// 因此这里直接查原始字节：每个 /Title 都必须以 FE FF 开头。
func TestPdfBookmarkTitlesAreUTF16InRawBytes(t *testing.T) {
	dir := t.TempDir()

	var images []string
	for i := 1; i <= 3; i++ {
		p := filepath.Join(dir, padName(i)+".png")
		makeTestImage(t, p, 200, 300)
		images = append(images, p)
	}

	out := filepath.Join(dir, "titles.pdf")
	if err := GeneratePdfFromImages(images, out, PDFOptions{
		Chapter:          "第1章",
		PerImageBookmark: true,
	}); err != nil {
		t.Fatalf("生成 PDF 失败: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	matches := titleLiteralRe.FindAllSubmatch(raw, -1)
	// 1 个章节书签 + 3 个页码书签
	if len(matches) != 4 {
		t.Fatalf("/Title 数量 = %d, 期望 4（1 章节 + 3 页码）", len(matches))
	}

	titles := map[string]bool{}
	for _, m := range matches {
		body := m[1]
		if !bytes.HasPrefix(body, []byte{0xFE, 0xFF}) {
			t.Errorf("书签标题没有 UTF-16BE BOM，真实阅读器会显示成乱码: % x", body)
			continue
		}
		// 解出来核对内容（顺带确认没有多编码/少编码）。
		units := make([]uint16, 0, (len(body)-2)/2)
		for i := 2; i+1 < len(body); i += 2 {
			units = append(units, uint16(body[i])<<8|uint16(body[i+1]))
		}
		titles[string(utf16.Decode(units))] = true
	}

	for _, want := range []string{"第1章", "1", "2", "3"} {
		if !titles[want] {
			t.Errorf("缺少书签 %q（解出的全部: %v）", want, titles)
		}
	}
}

// TestPerImageBookmarkFalseKeepsOnlyChapter 确认开关真的只是"不加二级书签"，
// 而不是把整棵大纲一起干掉 —— 关掉页码书签是宿主应对阅读器卡顿的手段，
// 章节书签必须留下。
func TestPerImageBookmarkFalseKeepsOnlyChapter(t *testing.T) {
	dir := t.TempDir()

	var images []string
	for i := 1; i <= 4; i++ {
		p := filepath.Join(dir, padName(i)+".png")
		makeTestImage(t, p, 200, 300)
		images = append(images, p)
	}

	out := filepath.Join(dir, "no_per_image.pdf")
	if err := GeneratePdfFromImages(images, out, PDFOptions{
		Chapter:          "第9章",
		PerImageBookmark: false,
	}); err != nil {
		t.Fatalf("生成 PDF 失败: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(titleLiteralRe.FindAllSubmatch(raw, -1)); n != 1 {
		t.Errorf("/Title 数量 = %d, 期望 1（只有章节书签）", n)
	}

	// 具体到内容：章节书签还得在，且编码正确。
	// 用 pdfcpu 读回来当交叉验证（它至少能解出我们写的内容）。
	titles, err := api.ListBookmarksFile(out, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("读取书签失败: %v", err)
	}
	if !containsSubstring(titles, "第9章") {
		t.Errorf("章节书签丢了: %q", titles)
	}
	if containsSubstring(titles, "1") {
		t.Errorf("per_image_bookmark=false 时不应有页码书签: %q", titles)
	}
}

// TestMergedPdfKeepsChapterAndPageBookmarks 确认合并（pdfcpu preserve 模式）
// 之后书签树仍然完整：5 章 × 各自页码。
//
// 注意合并**保持嵌套**：第1章在根，页码挂在它下面。之前踩过的坑是
// gofpdi 合并会把整棵大纲丢干净（只剩 0 个书签），所以模式不能换回默认。
func TestMergedPdfKeepsChapterAndPageBookmarks(t *testing.T) {
	dir := t.TempDir()

	var chapterPDFs []string
	for c := 1; c <= 2; c++ {
		var images []string
		pages := 3 + c
		for i := 1; i <= pages; i++ {
			p := filepath.Join(dir, itoaTest(c)+"-"+padName(i)+".png")
			makeTestImage(t, p, 200, 300)
			images = append(images, p)
		}

		pdfPath := filepath.Join(dir, "ch"+itoaTest(c)+".pdf")
		if err := GeneratePdfFromImages(images, pdfPath, PDFOptions{
			Chapter:          "第" + itoaTest(c) + "章",
			PerImageBookmark: true,
		}); err != nil {
			t.Fatalf("生成第 %d 章 PDF 失败: %v", c, err)
		}
		chapterPDFs = append(chapterPDFs, pdfPath)
	}

	merged := filepath.Join(dir, "merged.pdf")
	if err := MergePDFs(chapterPDFs, merged); err != nil {
		t.Fatalf("合并失败: %v", err)
	}

	f, err := os.Open(merged)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	bms, err := api.Bookmarks(f, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("读取合并后的书签失败: %v", err)
	}

	// 递归数一遍总数，并确认层级：根 = 章节，子 = 页码。
	if len(bms) != 2 {
		t.Fatalf("根书签数 = %d (%v), 期望 2 个章节", len(bms), bms)
	}
	total := 0
	for _, bm := range bms {
		if len(bm.Kids) == 0 {
			t.Errorf("章节 %q 下面没有页码书签 —— 合并把子节点丢了", bm.Title)
		}
		total += 1 + len(bm.Kids)
	}
	// 2 章节 + (4 + 5) 页码
	if total != 11 {
		t.Errorf("书签总数 = %d，期望 11（2 章节 + 4 + 5 页码）", total)
	}

	// 原始字节再兜一层：合并是 pdfcpu 重写的，别让它把编码搞坏。
	rawBytes, err := os.ReadFile(merged)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range titleLiteralRe.FindAllSubmatch(rawBytes, -1) {
		if !bytes.HasPrefix(m[1], []byte{0xFE, 0xFF}) {
			t.Errorf("合并后书名签丢了 BOM（阅读器会乱码）: % x", m[1])
		}
	}
	// 章节名是这里唯一的非 ASCII 内容，确认它真的以 UTF-16 形态存在于文件里。
	if !bytes.Contains(rawBytes, []byte{0xFE, 0xFF, 0x7B, 0x2C}) {
		t.Error("合并后的文件里找不到 UTF-16BE 编码的「第」，说明书名签被改写坏了")
	}
}

// TestSortImageNamesStillStripsZerosForOrdering 说明 SortImageNames 里的去零
// 是**另一码事**（只为让 10 排在 2 后面），不要和 bookmarkName 的显示去零混为一谈。
func TestSortImageNamesStillStripsZerosForOrdering(t *testing.T) {
	names := []string{"00010.jpg", "00002.jpg", "00001.jpg"}
	SortImageNames(names)
	want := []string{"00001.jpg", "00002.jpg", "00010.jpg"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("排序结果 = %v, 期望 %v", names, want)
		}
	}
}
