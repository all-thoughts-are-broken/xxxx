package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
)

// TestSanitizeFileName 覆盖各平台的非法字符与几个易踩的坑。
func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		"普通章节名":                "普通章节名",
		"第 1 章":                "第 1 章",
		"a/b\\c":               "a_b_c",
		`bad:name*with?"chars`: "bad_name_with__chars",
		"带<尖括号>的":              "带_尖括号_的",
		"  首尾空格  ":             "首尾空格", // 首尾空白被去掉
		"结尾是点...":              "结尾是点", // 尾部的点会被去掉
		"":                     "",
		"///":                  "",
		"CON":                  "_CON", // Windows 保留设备名
		"nul.txt":              "_nul.txt",
	}
	for in, want := range cases {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, 期望 %q", in, got, want)
		}
	}

	// 超长名字必须被截断（否则 Windows 上会建不出文件）
	long := ""
	for i := 0; i < 300; i++ {
		long += "长"
	}
	if got := sanitizeFileName(long); len([]rune(got)) > 80 {
		t.Errorf("超长文件名未截断，长度 %d", len([]rune(got)))
	}

	// 控制字符应被丢弃
	if got := sanitizeFileName("带\x00控制\x1f字符"); got != "带控制字符" {
		t.Errorf("控制字符未被丢弃: %q", got)
	}
}

// TestSafeChapterFileName 确认章节名为空时有兜底。
func TestSafeChapterFileName(t *testing.T) {
	if got := safeChapterFileName("", 12345); got != "chapter_12345" {
		t.Errorf("空章节名的兜底 = %q, 期望 chapter_12345", got)
	}
	if got := safeChapterFileName("//", 7); got != "chapter_7" {
		t.Errorf("全是非法字符时应当兜底，实际 %q", got)
	}
	if got := safeChapterFileName("第一话", 7); got != "第一话" {
		t.Errorf("正常章节名被改动: %q", got)
	}
}

// TestNormalizeRestoreExt 确认还原格式的归一化与默认值。
func TestNormalizeRestoreExt(t *testing.T) {
	cases := map[string]string{
		"":       ".jpg",
		"jpg":    ".jpg",
		"JPG":    ".jpg",
		"jpeg":   ".jpg",
		".jpeg":  ".jpg", // 带点也接受
		"png":    ".png",
		"PNG":    ".png",
		"webp":   ".jpg", // 不支持 → 退回默认 JPEG
		"avif":   ".jpg",
		"  jpg ": ".jpg",
	}
	for in, want := range cases {
		if got := normalizeRestoreExt(in); got != want {
			t.Errorf("normalizeRestoreExt(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestBuildPagePlan 覆盖页码/文件名/重名三类情况。
func TestBuildPagePlan(t *testing.T) {
	page := &client.ReadPageResult{
		Id: 399054,
		Images: []client.Images{
			{Page: 0, Image: "https://cdn.x.cc/media/photos/399054/00001.webp?t=178"}, // Page 缺省 → 用下标
			{Page: 2, Image: "https://cdn.x.cc/media/photos/399054/00002.webp?t=178"},
			{Page: 3, Image: ""}, // 空 URL 应当被跳过
			{Page: 4, Image: "https://cdn.x.cc/media/photos/399054/00004.webp"},
		},
	}

	plan := buildPagePlan(page)

	if len(plan) != 3 {
		t.Fatalf("计划条目 = %d, 期望 3（空 URL 的那页应被跳过）", len(plan))
	}

	// 第 0 项无 Page，用下标 1 兜底
	if plan[0].page != 1 {
		t.Errorf("Page 缺省时页码 = %d, 期望 1", plan[0].page)
	}
	// 文件名必须保留服务端命名（段数计算依赖它）
	if plan[0].pageName != "00001" {
		t.Errorf("pageName = %q, 期望 00001", plan[0].pageName)
	}
	if plan[0].rawName != "00001.webp" {
		t.Errorf("rawName = %q, 期望 00001.webp（query 必须去掉）", plan[0].rawName)
	}
	if plan[0].url == "" {
		t.Errorf("url 不应为空")
	}

	if plan[2].pageName != "00004" {
		t.Errorf("第 3 项 pageName = %q, 期望 00004", plan[2].pageName)
	}
}

// TestBuildPagePlanDedup 确认服务端偶发重名时不会互相覆盖。
func TestBuildPagePlanDedup(t *testing.T) {
	page := &client.ReadPageResult{
		Images: []client.Images{
			{Page: 1, Image: "https://c.cc/a/00001.webp"},
			{Page: 2, Image: "https://c.cc/b/00001.webp"}, // 同名不同目录
		},
	}

	plan := buildPagePlan(page)
	if len(plan) != 2 {
		t.Fatalf("计划条目 = %d, 期望 2", len(plan))
	}
	if plan[0].rawName == plan[1].rawName {
		t.Fatalf("重名未被处理，两个原始文件名都是 %q", plan[0].rawName)
	}
	// 页码仍然各自独立
	if plan[0].page != 1 || plan[1].page != 2 {
		t.Errorf("页码被弄乱了: %d / %d", plan[0].page, plan[1].page)
	}
}

// TestBuildPagePlanFallbackName 确认 URL 取不出文件名时用页码兜底。
func TestBuildPagePlanFallbackName(t *testing.T) {
	page := &client.ReadPageResult{
		Images: []client.Images{
			{Page: 7, Image: "https://c.cc/"},
		},
	}
	plan := buildPagePlan(page)
	if len(plan) != 1 {
		t.Fatalf("计划条目 = %d, 期望 1", len(plan))
	}
	if plan[0].pageName != "00007" {
		t.Errorf("兜底文件名 = %q, 期望 00007", plan[0].pageName)
	}
	if plan[0].rawName != "00007.webp" {
		t.Errorf("兜底原始文件名 = %q, 期望 00007.webp", plan[0].rawName)
	}
}

// TestPagePlanOutExt 确认 GIF 保持 gif，其余用默认格式。
//
// 这是「GIF 不还原也不转码」这条规则的落地点：GIF 是动画格式，
// 转成 JPEG 会丢帧，而 gofpdf 原生支持 gif。
func TestPagePlanOutExt(t *testing.T) {
	gif := pagePlan{rawName: "00001.gif", pageName: "00001"}
	if !gif.isGIF() {
		t.Errorf("isGIF 判断错误")
	}
	if got := gif.outExt(".jpg"); got != ".gif" {
		t.Errorf("GIF 输出扩展名 = %q, 期望 .gif", got)
	}

	webp := pagePlan{rawName: "00002.webp", pageName: "00002"}
	if webp.isGIF() {
		t.Errorf("webp 被误判为 GIF")
	}
	if got := webp.outExt(".jpg"); got != ".jpg" {
		t.Errorf("webp 输出扩展名 = %q, 期望 .jpg", got)
	}
	if got := webp.outExt(".png"); got != ".png" {
		t.Errorf("指定 png 时输出扩展名 = %q, 期望 .png", got)
	}
}

// TestRawNameFromURL 覆盖从直链抽文件名。
func TestRawNameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://c.cc/media/photos/1/00011.webp":       "00011.webp",
		"https://c.cc/media/photos/1/00011.webp?t=178": "00011.webp",
		"https://c.cc/media/photos/1/00011.webp#frag":  "00011.webp",
		`https://c.cc/media\photos\00011.webp`:         "00011.webp",
		"00011.webp":                                   "00011.webp",
		"https://c.cc/media/photos/1/":                 "",
		"https://c.cc/media/bad<name>.webp":            "",
	}
	for in, want := range cases {
		if got := rawNameFromURL(in); got != want {
			t.Errorf("rawNameFromURL(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestToAlbumDetail 确认接口响应的归一化。
func TestToAlbumDetail(t *testing.T) {
	raw := &client.AlbumDetailResult{
		Id:           1423323,
		Name:         "  我的偷渡日记  ",
		Description:  "**加粗**的简介\n换行",
		Likes:        "1234",
		TotalViews:   "98765",
		CommentTotal: "42",
		Addtime:      "1783091202",
		Author:       []string{"作者甲"},
		Tags:         []string{"标签1", "标签2"},
		Series: []client.Series{
			{Id: "1423324", Name: "第二话", Sort: "2"},
			{Id: "1423323", Name: "", Sort: "1"}, // 名字为空 → 补「第 1 章」
			{Id: "1423325", Name: "第三话", Sort: "3"},
		},
	}

	got := toAlbumDetail(raw, "cdn.x.cc")

	if got.Name != "我的偷渡日记" {
		t.Errorf("Name = %q，首尾空白未清理", got.Name)
	}
	if got.Likes != 1234 || got.Views != 98765 || got.Comments != 42 {
		t.Errorf("计数字段未转成整数: likes=%d views=%d comments=%d", got.Likes, got.Views, got.Comments)
	}
	if got.AddTime == "" || got.AddTime == "1783091202" {
		t.Errorf("AddTime 未被格式化成可读时间: %q", got.AddTime)
	}
	if got.CoverURL != "https://cdn.x.cc/media/albums/1423323_3x4.jpg" {
		t.Errorf("CoverURL = %q", got.CoverURL)
	}
	// Markdown 标记应被剥离
	if got.Description == raw.Description {
		t.Errorf("Description 未剥离 Markdown: %q", got.Description)
	}

	// 章节应按 sort 排序，且空名字要补
	if len(got.Chapters) != 3 {
		t.Fatalf("章节数 = %d, 期望 3", len(got.Chapters))
	}
	for i, want := range []int{1, 2, 3} {
		if got.Chapters[i].Sort != want {
			t.Errorf("第 %d 个章节 sort = %d, 期望 %d（章节未排序）", i, got.Chapters[i].Sort, want)
		}
	}
	if got.Chapters[0].Name != "第1章" {
		t.Errorf("空章节名未兜底: %q", got.Chapters[0].Name)
	}
}

// TestToAlbumDetailRedirect 确认「传章节号被重定向到作品」时如实记录。
func TestToAlbumDetailRedirect(t *testing.T) {
	// 请求 1423324（章节号），服务端返回作品 1423323
	got := toAlbumDetail(&client.AlbumDetailResult{
		Id:          1423323,
		RequestedId: 1423324,
	}, "")

	if got.RedirectedFrom != 1423324 {
		t.Errorf("RedirectedFrom = %d, 期望 1423324", got.RedirectedFrom)
	}

	// 没有重定向时不该有值
	plain := toAlbumDetail(&client.AlbumDetailResult{
		Id: 1423323, RequestedId: 1423323,
	}, "")
	if plain.RedirectedFrom != 0 {
		t.Errorf("无重定向时 RedirectedFrom = %d, 期望 0", plain.RedirectedFrom)
	}
}

// TestSortChapters 确认排序对乱序、缺 sort、重复 sort 都稳健。
//
// 需要留意的语义：接口没给序号（Sort<=0）的章节整体挪到末尾，
// 并保持它们彼此之间的原有顺序。
func TestSortChapters(t *testing.T) {
	chs := []ChapterBrief{
		{ID: 5, Sort: 5},
		{ID: 2, Sort: 2},
		{ID: 0, Sort: 0}, // 没有 sort 的应整体挪到末尾
		{ID: 1, Sort: 1},
	}
	sortChapters(chs)

	if len(chs) != 4 {
		t.Fatalf("排序过程丢了元素: %d", len(chs))
	}

	// 有序号的部分必须严格升序
	if chs[0].Sort != 1 || chs[1].Sort != 2 || chs[2].Sort != 5 {
		t.Errorf("排序结果 = %v, 期望 1,2,5",
			[]int{chs[0].Sort, chs[1].Sort, chs[2].Sort})
	}
	// 无序号的必须落在末尾
	if chs[3].Sort != 0 || chs[3].ID != 0 {
		t.Errorf("无序号的章节没有落到末尾: %+v", chs[3])
	}

	// 无序号元素之间的相对顺序应保持（稳定性）
	mixed := []ChapterBrief{
		{ID: 100, Sort: 0},
		{ID: 7, Sort: 7},
		{ID: 200, Sort: 0},
		{ID: 3, Sort: 3},
	}
	sortChapters(mixed)
	if mixed[0].Sort != 3 || mixed[1].Sort != 7 {
		t.Errorf("有序号元素排序错误: %+v", mixed)
	}
	if mixed[2].ID != 100 || mixed[3].ID != 200 {
		t.Errorf("无序号元素的相对顺序被破坏: %+v", mixed)
	}
}

// TestReadingIndex 确认高亮章节的判定。
func TestReadingIndex(t *testing.T) {
	chs := []ChapterBrief{{ID: 10}, {ID: 20}, {ID: 30}}

	if got := readingIndex(chs, 20); got != 2 {
		t.Errorf("命中第 2 章时 = %d, 期望 2", got)
	}
	if got := readingIndex(chs, 99); got != 0 {
		t.Errorf("未命中时应为 0, 实际 %d", got)
	}
	if got := readingIndex(chs, 0); got != 0 {
		t.Errorf("id 为 0 时应为 0, 实际 %d", got)
	}
	// 负数 id 与空章节表也不能命中（前者不会与真实 id 相等，但显式挡掉更稳）。
	if got := readingIndex(chs, -1); got != 0 {
		t.Errorf("id 为负数时应为 0, 实际 %d", got)
	}
	if got := readingIndex(nil, 20); got != 0 {
		t.Errorf("没有章节时应为 0, 实际 %d", got)
	}
}

// TestApplyDirOverrides 确认目录覆盖只影响本次调用的副本。
func TestApplyDirOverrides(t *testing.T) {
	svc := &Service{cfg: config.NewStore(nil)}
	base := config.Default()
	base.DownloadDir = "raw-default"
	base.OutputDir = "out-default"

	req := &DownloadAlbumRequest{RawDir: "  G:/tmp/raw  ", OutputDir: "G:/tmp/out"}
	got := svc.applyDirOverrides(base, req)

	if got.DownloadDir != "G:/tmp/raw" {
		t.Errorf("DownloadDir = %q（应去空白）", got.DownloadDir)
	}
	if got.OutputDir != "G:/tmp/out" {
		t.Errorf("OutputDir = %q", got.OutputDir)
	}
	// 原配置不能被改
	if base.DownloadDir != "raw-default" || base.OutputDir != "out-default" {
		t.Errorf("原配置被污染了: %q / %q", base.DownloadDir, base.OutputDir)
	}

	// 不传覆盖时用默认值
	got2 := svc.applyDirOverrides(base, &DownloadAlbumRequest{})
	if got2.DownloadDir != "raw-default" {
		t.Errorf("无覆盖时 DownloadDir = %q", got2.DownloadDir)
	}
}

// TestSamePath 确认「就地加密」的路径判定。
func TestSamePath(t *testing.T) {
	if !samePath("a.pdf", "a.pdf") {
		t.Errorf("相同路径被判为不同")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "x.pdf")
	// 同一路径的不同写法应当判为相同
	if !samePath(p, filepath.Join(dir, ".", "x.pdf")) {
		t.Errorf("等价路径未判为相同: %q vs %q", p, filepath.Join(dir, ".", "x.pdf"))
	}
	if samePath(p, filepath.Join(dir, "y.pdf")) {
		t.Errorf("不同文件被判为相同")
	}
}

// TestResolveImagesFromDir 确认目录扫描只取图片并按自然序排序。
func TestResolveImagesFromDir(t *testing.T) {
	dir := t.TempDir()

	// 故意制造字典序会排错的场景：10 应当排在 2 之后
	for _, n := range []string{"10.jpg", "2.jpg", "1.jpg", "readme.txt", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 子目录必须被忽略
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "9.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &Service{cfg: config.NewStore(nil)}
	got, err := svc.resolveImages(ConvertToPDFRequest{InputDir: dir})
	if err != nil {
		t.Fatalf("resolveImages 失败: %v", err)
	}

	var names []string
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}

	want := []string{"1.jpg", "2.jpg", "10.jpg"}
	if len(names) != len(want) {
		t.Fatalf("图片数 = %d (%v), 期望 %d", len(names), names, len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("第 %d 个 = %q, 期望 %q（自然序: %v）", i, names[i], want[i], names)
		}
	}
}

// TestResolveImagesExplicitRejectsNonImage 确认显式列表会拒绝非图片。
func TestResolveImagesExplicitRejectsNonImage(t *testing.T) {
	svc := &Service{cfg: config.NewStore(nil)}

	_, err := svc.resolveImages(ConvertToPDFRequest{Images: []string{"a.txt"}})
	if err == nil {
		t.Fatalf("显式列表里的非图片应当被拒绝")
	}

	got, err := svc.resolveImages(ConvertToPDFRequest{Images: []string{" a.jpg ", "", "b.png"}})
	if err != nil {
		t.Fatalf("合法输入不应报错: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("空字符串应被跳过，实际 %d 个: %v", len(got), got)
	}
}

// TestDownloaderCacheReuse 确认下载器按配置指纹复用（连接池复用是性能关键）。
func TestDownloaderCacheReuse(t *testing.T) {
	svc := &Service{cfg: config.NewStore(nil)}

	cfgA := config.Default()
	cfgA.Proxy = ""
	cfgA.TimeoutSec = 30

	d1 := svc.downloaderFor(cfgA)
	d2 := svc.downloaderFor(cfgA)
	if d1 != d2 {
		t.Errorf("相同配置未复用下载器（会丢掉连接池）")
	}

	// 换代理必须换实例
	cfgB := *cfgA
	cfgB.Proxy = "http://127.0.0.1:8080"
	d3 := svc.downloaderFor(&cfgB)
	if d3 == d1 {
		t.Errorf("代理变化后仍复用了旧下载器")
	}
}

// TestClientKeyChangesWithNetworkConfig 确认网络配置指纹能感知每一项变化。
func TestClientKeyChangesWithNetworkConfig(t *testing.T) {
	base := config.Default()
	baseKey := clientKey(base)

	mutations := map[string]func(*config.Config){
		"base_url":    func(c *config.Config) { c.BaseURL = "https://x.cc" },
		"secret":      func(c *config.Config) { c.Secret = "另一个" },
		"app_version": func(c *config.Config) { c.AppVersion = "9.9.9" },
		"user_agent":  func(c *config.Config) { c.UserAgent = "UA" },
		"proxy":       func(c *config.Config) { c.Proxy = "http://127.0.0.1:1" },
		"timeout":     func(c *config.Config) { c.TimeoutSec = 99 },
		"cdn_host":    func(c *config.Config) { c.CDNHost = "cdn.y.cc" },
	}

	for name, mutate := range mutations {
		cp := *base
		mutate(&cp)
		if clientKey(&cp) == baseKey {
			t.Errorf("改动 %s 后指纹没变，client 不会被重建", name)
		}
	}
}

// TestChapterNameOf / detailToChapters 只走一遍主要路径。
func TestDetailToChapters(t *testing.T) {
	detail := &client.AlbumDetailResult{
		Series: []client.Series{
			{Id: "0", Name: "非法 id", Sort: "1"}, // id 非法 → 跳过
			{Id: "77", Name: "第二话", Sort: "2"},
			{Id: "55", Name: "第一话", Sort: "1"},
		},
	}
	got := detailToChapters(detail)
	if len(got) != 2 {
		t.Fatalf("章节数 = %d, 期望 2（非法 id 应被跳过）", len(got))
	}
	if got[0].ID != 55 || got[1].ID != 77 {
		t.Errorf("章节未按 sort 排序: %+v", got)
	}

	svc := &Service{cfg: config.NewStore(nil)}
	if name := svc.chapterNameOf(detail, 77); name != "第二话" {
		t.Errorf("chapterNameOf(77) = %q", name)
	}
	if name := svc.chapterNameOf(detail, 999); name != "" {
		t.Errorf("未知章节应返回空串, 实际 %q", name)
	}
}
