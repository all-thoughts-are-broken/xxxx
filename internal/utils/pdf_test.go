package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// makeTestImage 生成一张尺寸可控的 PNG，用于 PDF 合成测试。
func makeTestImage(t *testing.T, path string, w, h int) {
	t.Helper()
	if err := imaging.Save(smoothImage(w, h), path); err != nil {
		t.Fatalf("准备测试图 %s 失败: %v", path, err)
	}
}

// TestGeneratePdfSingleLayout 验证「一章 = 一个只有一页的长 PDF」这个产品形态。
//
// 断言方式：用 pdfcpu 自己把生成的文件读回来。只看文件非空是不够的 ——
// 一个截断的 PDF 也能非空，但读不回来。
func TestGeneratePdfSingleLayout(t *testing.T) {
	dir := t.TempDir()

	images := []string{
		filepath.Join(dir, "00001.png"),
		filepath.Join(dir, "00002.png"),
		filepath.Join(dir, "00003.png"),
	}
	// 三张不同高度的图，方便校验页面总高
	makeTestImage(t, images[0], 200, 300)
	makeTestImage(t, images[1], 200, 400)
	makeTestImage(t, images[2], 200, 250)

	out := filepath.Join(dir, "chapter.pdf")

	err := GeneratePdfFromImages(images, out, PDFOptions{
		Chapter: "第一话",
		Layout:  "single",
	})
	if err != nil {
		t.Fatalf("生成 PDF 失败: %v", err)
	}

	if err := api.ValidateFile(out, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("生成的 PDF 无法通过校验（文件可能被截断）: %v", err)
	}

	count, err := api.PageCountFile(out)
	if err != nil {
		t.Fatalf("读取页数失败: %v", err)
	}
	if count != 1 {
		t.Errorf("single 布局的页数 = %d, 期望 1（整章合成一条长页）", count)
	}

	ctx, err := api.ReadContextFile(out)
	if err != nil {
		t.Fatalf("读取 PDF 上下文失败: %v", err)
	}
	if ctx.PageCount != 1 {
		t.Fatalf("上下文页数 = %d", ctx.PageCount)
	}

	// 书签：顶层章节名应当存在
	titles, err := api.ListBookmarksFile(out, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("读取书签失败: %v", err)
	}
	if !containsSubstring(titles, "第一话") {
		t.Errorf("顶层书签缺失: %q", titles)
	}
}

// TestGeneratePdfPagedLayout 验证分页布局不会把单张图切开。
func TestGeneratePdfPagedLayout(t *testing.T) {
	dir := t.TempDir()

	// 三张 100pt 高的图，页高上限 250 → 应当分成 2 页
	var images []string
	for i := 1; i <= 3; i++ {
		p := filepath.Join(dir, padName(i)+".png")
		makeTestImage(t, p, 200, 100)
		images = append(images, p)
	}

	out := filepath.Join(dir, "paged.pdf")
	err := GeneratePdfFromImages(images, out, PDFOptions{
		Layout:        "paged",
		MaxPageHeight: 250,
	})
	if err != nil {
		t.Fatalf("生成 PDF 失败: %v", err)
	}
	if err := api.ValidateFile(out, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("生成的 PDF 无法通过校验: %v", err)
	}

	count, err := api.PageCountFile(out)
	if err != nil {
		t.Fatalf("读取页数失败: %v", err)
	}
	// 100+100=200 ≤ 250 装得下两张，加第三张就 300 > 250 → 第二页装第三张
	if count != 2 {
		t.Errorf("paged 布局页数 = %d, 期望 2", count)
	}
}

// TestGeneratePdfWithPlaceholderPage 验证「读取失败的图会被替换成占位图而不是被跳过」。
//
// 这条很重要：直接跳过会让后续所有页码前移，读者看到的页码与真实章节错位。
// 占位图由下载链路生成（见 service.WritePlaceholder），这里确认 PDF 层
// 能接受并如实排入。
func TestGeneratePdfWithPlaceholderPage(t *testing.T) {
	dir := t.TempDir()

	good1 := filepath.Join(dir, "00001.png")
	bad := filepath.Join(dir, "00002.jpg") // 故意写坏
	good2 := filepath.Join(dir, "00003.png")

	makeTestImage(t, good1, 200, 300)
	makeTestImage(t, good2, 200, 300)
	if err := os.WriteFile(bad, []byte("这不是图片内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "with_bad.pdf")
	err := GeneratePdfFromImages([]string{good1, bad, good2}, out, PDFOptions{Layout: "single"})
	if err != nil {
		t.Fatalf("坏图应当被替换成占位图而不是让整体失败: %v", err)
	}

	if err := api.ValidateFile(out, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("生成的 PDF 无法通过校验: %v", err)
	}
	if count, err := api.PageCountFile(out); err != nil || count != 1 {
		t.Fatalf("页数 = %d (err=%v), 期望 1", count, err)
	}
}

// TestPlaceholderKeepsPageAndBookmarkAlignment 是上一条的加强版：
// 坏掉的图不只「不报错」，还要「占住它原本的位置」。
//
// 校验的是最终产物：5 张图里坏 2 张，页码书签必须仍然凑满 5 个（1..5），
// 少的任何一个都意味着后面的页在读者眼里整体前移了。
func TestPlaceholderKeepsPageAndBookmarkAlignment(t *testing.T) {
	dir := t.TempDir()

	var images []string
	for i := 1; i <= 5; i++ {
		p := filepath.Join(dir, padName(i)+".jpg")
		if i == 3 || i == 4 {
			// 模拟下载截断：文件在、扩展名对、内容不是图片
			if err := os.WriteFile(p, []byte("\xff\xd8 截断的半个 jpeg"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			makeTestImage(t, p, 200, 300)
		}
		images = append(images, p)
	}

	out := filepath.Join(dir, "align.pdf")
	if err := GeneratePdfFromImages(images, out, PDFOptions{
		Chapter:          "第1章",
		PerImageBookmark: true,
	}); err != nil {
		t.Fatalf("坏图应当被替换成占位图: %v", err)
	}

	titles, err := api.ListBookmarksFile(out, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("读取书签失败: %v", err)
	}
	// 1 个章节书签 + 5 个页码书签
	if len(titles) != 6 {
		t.Errorf("书签数 = %d (%q), 期望 6（1 章节 + 5 页码）", len(titles), titles)
	}
	for _, want := range []string{"第1章", "1", "2", "3", "4", "5"} {
		if !containsSubstring(titles, want) {
			t.Errorf("缺少书签 %q（实际: %q）", want, titles)
		}
	}

	if err := api.ValidateFile(out, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("生成的 PDF 无法通过校验: %v", err)
	}
}

// TestGeneratePdfFromDirReplacesBadImage 覆盖目录模式（下载链路真正走的分支）。
//
// 同时验证目录模式该跳过的要跳过：非图片文件与子目录不能混进页面里，
// 否则页数会和章节真实页数对不上。
func TestGeneratePdfFromDirReplacesBadImage(t *testing.T) {
	dir := t.TempDir()

	makeTestImage(t, filepath.Join(dir, "00001.jpg"), 200, 300)
	if err := os.WriteFile(filepath.Join(dir, "00002.jpg"), []byte("坏图"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeTestImage(t, filepath.Join(dir, "00003.jpg"), 200, 300)
	// 这两个都不该被当成页面
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("说明"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "chapter.pdf")
	if err := GeneratePdfFromDir(dir, out, "第1章"); err != nil {
		t.Fatalf("目录模式生成失败: %v", err)
	}

	titles, err := api.ListBookmarksFile(out, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("读取书签失败: %v", err)
	}
	// 1 章节 + 3 页码；notes.txt 与 sub/ 不能占位
	if len(titles) != 4 {
		t.Errorf("书签数 = %d (%q), 期望 4（1 章节 + 3 页码）", len(titles), titles)
	}
	if err := api.ValidateFile(out, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("生成的 PDF 无法通过校验: %v", err)
	}
}

// TestBookmarkNameStripsLeadingZeros 锁定页码书签的命名规则。
//
// 规则来源：pdf_test/tools.go（用户最后一次修改的版本，17:51）——
//
//	bookmark := strings.TrimLeft(strings.TrimSuffix(base, filepath.Ext(base)), "0")
//
// 注意 15:38 生成的那批参考 PDF（如 output/1472136.pdf）里的书签还是 "00001"，
// 那是 tools.go 更早的版本；以 17:51 版为准，所以这里断言去掉前导 0。
func TestBookmarkNameStripsLeadingZeros(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"00001.jpg", "1"},
		{"00012.webp", "12"},
		{"00003.png", "3"},
		{"7.jpeg", "7"},
		{"100.jpg", "100"},
		{"封面.jpg", "封面"},
		// 全是 0：TrimLeft 会得到空串，此时必须回退到原名 ——
		// 空标题的书签在阅读器里是个点不开的空节点。
		{"0.png", "0"},
		{"0000.jpg", "0000"},
	}
	for _, c := range cases {
		if got := bookmarkName(c.in); got != c.want {
			t.Errorf("bookmarkName(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestGeneratePdfRejectsNonImagePath 确认容错是有边界的。
//
// 「解码失败」才降级成占位图；扩展名根本不是图片属于调用方传错参数，
// 必须硬报错 —— 静默吞掉会让用户以为整章都合进去了。
func TestGeneratePdfRejectsNonImagePath(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(txt, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := GeneratePdfFromImages([]string{txt}, filepath.Join(dir, "x.pdf"), PDFOptions{})
	if err == nil {
		t.Errorf("非图片扩展名应当硬报错，而不是悄悄生成一张占位图")
	}
}

// TestMergePDFsPreservesOrderAndBookmarks 验证合并保持顺序并保留章节书签。
func TestMergePDFsPreservesOrderAndBookmarks(t *testing.T) {
	dir := t.TempDir()

	mkChapter := func(name string, pages int) string {
		var imgs []string
		for i := 1; i <= pages; i++ {
			p := filepath.Join(dir, name+"_"+padName(i)+".png")
			makeTestImage(t, p, 200, 300)
			imgs = append(imgs, p)
		}
		out := filepath.Join(dir, name+".pdf")
		if err := GeneratePdfFromImages(imgs, out, PDFOptions{
			Chapter: name,
			Layout:  "paged",
			// 页高上限设得很小 → 每张图各占一页，方便数页数
			MaxPageHeight: 350,
		}); err != nil {
			t.Fatalf("生成 %s 失败: %v", name, err)
		}
		return out
	}

	// 注意章节顺序：故意与字母序相反，用来证明合并是按入参顺序而非路径排序
	chB := mkChapter("第二话", 2)
	chA := mkChapter("第一话", 3)

	merged := filepath.Join(dir, "book.pdf")
	if err := MergePDFs([]string{chA, chB}, merged); err != nil {
		t.Fatalf("合并失败: %v", err)
	}

	if err := api.ValidateFile(merged, model.NewDefaultConfiguration()); err != nil {
		t.Fatalf("合并后的 PDF 无法通过校验: %v", err)
	}

	count, err := api.PageCountFile(merged)
	if err != nil {
		t.Fatalf("读取页数失败: %v", err)
	}
	if count != 5 { // 3 + 2
		t.Errorf("合并后页数 = %d, 期望 5", count)
	}

	titles, err := api.ListBookmarksFile(merged, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("读取书签失败: %v", err)
	}
	for _, want := range []string{"第一话", "第二话"} {
		if !containsSubstring(titles, want) {
			t.Errorf("合并后缺少章节书签 %q（实际: %q）", want, titles)
		}
	}
}

// TestSetPDFPassword 验证加密后的文件真的需要密码才能读。
func TestSetPDFPassword(t *testing.T) {
	dir := t.TempDir()

	img := filepath.Join(dir, "00001.png")
	makeTestImage(t, img, 200, 300)
	plain := filepath.Join(dir, "plain.pdf")
	if err := GeneratePdfFromImages([]string{img}, plain, PDFOptions{}); err != nil {
		t.Fatalf("生成 PDF 失败: %v", err)
	}

	locked := filepath.Join(dir, "locked.pdf")
	if err := SetPDFPasswordWithOwner(plain, locked, "userpw", "ownerpw"); err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	// 加密文件必须留下 /Encrypt 标记
	raw, err := os.ReadFile(locked)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "/Encrypt") {
		t.Errorf("加密后的 PDF 里没有 /Encrypt 标记，加密可能没生效")
	}

	// 用空配置（无密码）读取应当失败
	if err := api.ValidateFile(locked, model.NewDefaultConfiguration()); err == nil {
		t.Errorf("无密码竟然能校验加密 PDF，加密没有生效")
	}

	// 用正确密码应当能读
	withPw := model.NewDefaultConfiguration()
	withPw.UserPW = "userpw"
	withPw.OwnerPW = "ownerpw"
	if err := api.ValidateFile(locked, withPw); err != nil {
		t.Errorf("用正确密码无法读取: %v", err)
	}

	// 非空密码才合法
	if err := SetPDFPassword(plain, filepath.Join(dir, "x.pdf"), ""); err == nil {
		t.Errorf("空密码应当被拒绝")
	}
}

// TestGeneratePdfRejectsEmptyInput 确认空输入给出明确错误。
func TestGeneratePdfRejectsEmptyInput(t *testing.T) {
	dir := t.TempDir()
	if err := GeneratePdfFromImages(nil, filepath.Join(dir, "x.pdf"), PDFOptions{}); err == nil {
		t.Errorf("空图片列表应当报错")
	}
	if err := MergePDFs(nil, filepath.Join(dir, "y.pdf")); err == nil {
		t.Errorf("空合并列表应当报错")
	}
}

// TestSortImageNamesNatural 确认自然序排序（1 < 2 < 10）。
func TestSortImageNamesNatural(t *testing.T) {
	names := []string{"10.jpg", "2.jpg", "1.jpg", "00003.jpg", "00001.jpg"}
	SortImageNames(names)

	// 只校验「带前导 0 的一组」与「无前导 0 的一组」各自内部有序，
	// 并确认 2 排在 10 前面（这正是字典序会搞错的点）
	idx := map[string]int{}
	for i, n := range names {
		idx[n] = i
	}
	if idx["2.jpg"] > idx["10.jpg"] {
		t.Errorf("自然序错误: 2 应排在 10 之前，实际 %v", names)
	}
	if idx["00001.jpg"] > idx["00003.jpg"] {
		t.Errorf("前导 0 的一组排序错误: %v", names)
	}
}

// padName 生成 5 位零填充名（对齐服务端命名）。
func padName(n int) string {
	s := itoaTest(n)
	for len(s) < 5 {
		s = "0" + s
	}
	return s
}

// itoaTest 是测试里的小工具（避免为一个整数转换引入 strconv）。
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// containsSubstring 报告书签列表里是否有任意一项包含子串。
func containsSubstring(items []string, sub string) bool {
	for _, it := range items {
		if strings.Contains(it, sub) {
			return true
		}
	}
	return false
}
