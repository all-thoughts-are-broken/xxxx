package service

import (
	"context"
	"strings"
	"testing"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
)

// newTestService 造一个不碰网络的 Service。
func newTestService() *Service {
	return &Service{cfg: config.NewStore(nil)}
}

// plainCfg 返回一份关掉 CDN 的配置。
//
// CDNHost 为空时 toComment 不会调 fetchAsset，因此这些测试完全离线 ——
// 这一点本身就是被测契约之一（见 TestToCommentWithoutCDNHostSkipsAssets）。
func plainCfg() *config.Config {
	cfg := config.Default()
	cfg.CDNHost = ""
	return cfg
}

// ---- 评论树转换 ----

// TestToCommentMapsFields 评论条目 → 渲染层入参的字段映射。
//
// 昵称/正文/等级/点赞都要照搬，其中昵称走的是 trimUnicodeSpace：
// 接口返回的昵称经常带全角空格（U+3000），不去掉会在渲染层算错行宽。
func TestToCommentMapsFields(t *testing.T) {
	svc := newTestService()
	inline := map[string]string{"https://cdn/emoji/1.png": "/tmp/1.png"}

	raw := &client.List{
		Nickname: "\u3000姐姐\u3000", // 前后全角空格
		Username: "不该被用到",
		Content:  "正文内容",
		Addtime:  "2026-09-20 10:00:00",
		UID:      "42",
		Likes:    "7",
		Expinfo:  client.Expinfo{Level: 3, LevelName: "Lv.3"},
	}

	got := svc.toComment(context.Background(), plainCfg(), raw, inline)

	if got.Nickname != "姐姐" {
		t.Errorf("Nickname = %q，期望去掉全角空格的 %q", got.Nickname, "姐姐")
	}
	if got.Content != "正文内容" {
		t.Errorf("Content = %q", got.Content)
	}
	if got.AddTime != "2026-09-20 10:00:00" {
		t.Errorf("AddTime = %q", got.AddTime)
	}
	if got.UID != "42" {
		t.Errorf("UID = %q", got.UID)
	}
	if got.Likes != 7 {
		t.Errorf("Likes = %d，期望 7（字符串转数字）", got.Likes)
	}
	if got.Level != 3 || got.LevelName != "Lv.3" {
		t.Errorf("等级 = (%d, %q)，期望 (3, %q)", got.Level, got.LevelName, "Lv.3")
	}
	// 表情图整棵树共享同一份 map，渲染层按正文里出现的地址去查。
	if len(got.InlineImages) != 1 {
		t.Errorf("InlineImages 应原样带过去，实际 %v", got.InlineImages)
	}
}

// TestToCommentFallsBackToUsername 昵称为空（或全是空白）时退回 username。
//
// 服务端有些账号没设昵称，只有 username；不兜底就会渲染出一个无名楼层。
func TestToCommentFallsBackToUsername(t *testing.T) {
	svc := newTestService()

	for _, blank := range []string{"", "   ", "\u3000", "\t\n"} {
		raw := &client.List{Nickname: blank, Username: " 备用名 "}
		got := svc.toComment(context.Background(), plainCfg(), raw, nil)
		if got.Nickname != "备用名" {
			t.Errorf("nickname=%q 时应退回 username，实际 Nickname = %q", blank, got.Nickname)
		}
	}

	// 昵称非空时不该被 username 覆盖。
	raw := &client.List{Nickname: "真昵称", Username: "备用"}
	if got := svc.toComment(context.Background(), plainCfg(), raw, nil); got.Nickname != "真昵称" {
		t.Errorf("Nickname = %q，期望 %q", got.Nickname, "真昵称")
	}
}

// TestToCommentWithoutCDNHostSkipsAssets CDNHost 为空时完全不碰资源下载。
//
// 这条是有实际价值的：没有 CDN 域名就拼不出头像/勋章 URL，
// 此时必须**跳过**而不是拼一个 "https:///media/users/..." 出来 ——
// 后者会白白发起一轮注定失败的下载（CDNHost 为空通常意味着还没探活）。
func TestToCommentWithoutCDNHostSkipsAssets(t *testing.T) {
	svc := newTestService()
	cfg := plainCfg()

	raw := &client.List{
		Nickname: "某人",
		Photo:    "nopic-Male.gif",
		Expinfo: client.Expinfo{
			Badges: []client.Badge{{Id: "b1", Content: "/static/resources/images/x.png"}},
		},
	}

	got := svc.toComment(context.Background(), cfg, raw, nil)

	if got.AvatarPath != "" {
		t.Errorf("CDNHost 为空时不该去下载头像，实际 AvatarPath = %q", got.AvatarPath)
	}
	if len(got.Badges) != 0 {
		t.Errorf("CDNHost 为空时不该去下载勋章，实际 %v", got.Badges)
	}
}

// TestToCommentConvertsRepliesRecursively 楼中楼要递归转成 utils.Comment。
//
// ⚠️ 递归深度**恒为 2 层**（顶层评论 + 一层回复），因为 client.Reply 结构体
// 压根没有 Replys 字段 —— 服务端的评论树就是两层的。这条测试同时把这个事实
// 钉住：若哪天给 Reply 加了 Replys，就必须同时改 replyToList 把它带过去，
// 否则深层回复会被静默丢掉（渲染出来的楼层少一截，但不会报任何错）。
func TestToCommentConvertsRepliesRecursively(t *testing.T) {
	svc := newTestService()
	inline := map[string]string{"u": "/p"}

	replies := []client.Reply{
		{CID: "c1", Nickname: "回复甲", Content: "r1", Likes: "2"},
		{CID: "c2", Nickname: "", Username: "回复乙", Content: "r2", Likes: "x"},
	}
	raw := &client.List{
		Nickname: "楼主",
		Content:  "主楼",
		Replys:   &replies,
	}

	got := svc.toComment(context.Background(), plainCfg(), raw, inline)

	if len(got.Replies) != 2 {
		t.Fatalf("Replies 应有 2 条，实际 %d 条: %+v", len(got.Replies), got.Replies)
	}
	if got.Replies[0].Nickname != "回复甲" || got.Replies[0].Content != "r1" {
		t.Errorf("第一条回复 = %+v", got.Replies[0])
	}
	if got.Replies[0].Likes != 2 {
		t.Errorf("第一条回复 Likes = %d，期望 2", got.Replies[0].Likes)
	}
	// 回复的昵称同样要兜底到 username。
	if got.Replies[1].Nickname != "回复乙" {
		t.Errorf("第二条回复昵称 = %q，期望退回 username", got.Replies[1].Nickname)
	}
	// Likes="x" 非数字 → 0，而不是让整个渲染炸掉。
	if got.Replies[1].Likes != 0 {
		t.Errorf("非数字 Likes 应退成 0，实际 %d", got.Replies[1].Likes)
	}
	// 递归到此为止（Reply 没有 Replys 字段）。
	if got.Replies[0].Replies != nil {
		t.Errorf("递归深度应为 2 层，实际又往下钻了: %+v", got.Replies[0].Replies)
	}
	// 表情图整棵树共享同一份（不是每条回复各自一份）。
	if got.Replies[0].InlineImages["u"] != "/p" {
		t.Error("回复层应共享顶层预抓的表情图")
	}

	// Replys 为 nil 时不该出现空切片。
	if plain := svc.toComment(context.Background(), plainCfg(), &client.List{Nickname: "x"}, nil); plain.Replies != nil {
		t.Errorf("没有回复时应保持 nil，实际 %v", plain.Replies)
	}
}

// TestReplyToListMapsFields Reply → List 的适配要不丢字段。
//
// 这个适配器存在的意义是让 toComment 只认一种形状，所以它**必须**把
// 渲染用得到的字段都搬过去；漏掉一个就是"某类回复少了昵称/正文"。
// 注意 AID 不在其中：Reply 结构体里没有这个字段。
func TestReplyToListMapsFields(t *testing.T) {
	r := &client.Reply{
		CID:       "c9",
		UID:       "u9",
		Username:  "user9",
		Nickname:  "昵称9",
		Likes:     "13",
		Addtime:   "2026-09-01",
		ParentCID: "c0",
		Photo:     "p.gif",
		Content:   "回复正文",
		Expinfo:   client.Expinfo{Level: 5, LevelName: "Lv.5"},
	}

	got := replyToList(r)

	if got.CID != "c9" || got.UID != "u9" || got.Nickname != "昵称9" || got.Username != "user9" {
		t.Errorf("标识/昵称字段丢失: %+v", got)
	}
	if got.Likes != "13" || got.Addtime != "2026-09-01" {
		t.Errorf("点赞/时间丢失: %+v", got)
	}
	if got.Photo != "p.gif" || got.Content != "回复正文" {
		t.Errorf("头像/正文丢失: %+v", got)
	}
	if got.Expinfo.Level != 5 || got.Expinfo.LevelName != "Lv.5" {
		t.Errorf("等级信息丢失: %+v", got.Expinfo)
	}
	if got.AID != "" {
		t.Errorf("AID 应当留空（Reply 里没有这个字段），实际 %q", got.AID)
	}
	if got.Replys != nil {
		t.Errorf("Replys 应当留空（Reply 里没有这个字段），实际 %v", got.Replys)
	}
}

// ---- 进度回调 ----

// TestProgressFuncNilIsSafe 调用方不关心进度时，回调为 nil。
//
// 所有编排代码都直接 `progress.step(...)`，不会先判 nil —— 所以
// nil 接收者必须安全，否则每一处进度上报都要加判断。
func TestProgressFuncNilIsSafe(t *testing.T) {
	var nilFn ProgressFunc
	nilFn.report(Progress{Stage: "download"}) // 不该 panic
	nilFn.step("download", 1, 10, "x")
}

// TestProgressFuncStep 收齐 step 构造出来的字段。
//
// 进度是宿主唯一能看到"卡在哪一步"的渠道，字段漏了或串了会变成
// 界面上一根永远不动的进度条。
func TestProgressFuncStep(t *testing.T) {
	var got []Progress
	var fn ProgressFunc = func(p Progress) { got = append(got, p) }

	fn.step("download", 3, 10, "正在下载 3/10")
	fn.report(Progress{
		Stage: "pdf", Done: 1, Total: 1, Message: "完成",
		Extra: map[string]any{"pdf_path": "out.pdf"},
	})

	if len(got) != 2 {
		t.Fatalf("应收到 2 次进度，实际 %d 次", len(got))
	}
	if got[0].Stage != "download" || got[0].Done != 3 || got[0].Total != 10 {
		t.Errorf("step 构造的进度 = %+v", got[0])
	}
	if got[0].Message != "正在下载 3/10" {
		t.Errorf("Message = %q", got[0].Message)
	}
	if got[1].Extra["pdf_path"] != "out.pdf" {
		t.Errorf("Extra 丢失: %+v", got[1].Extra)
	}
}

// ---- 小工具 ----

// TestAtoiSafe 数字字符串的安全转换：非数字、负数、溢出都退成 0。
//
// 用在 likes 这类"接口偶尔给脏值"的字段上。退成 0 是可接受的降级
// （少显示一个点赞数），而让 strconv 的错误冒出去会整张图渲染失败。
func TestAtoiSafe(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"7", 7},
		{" 7 ", 7},
		{"0", 0},
		{"", 0},
		{"abc", 0},
		{"7.5", 0},
		{"-5", 0},                   // 负数非法
		{"99999999999999999999", 0}, // 溢出
		{"+12", 12},                 // strconv 接受前导 +
		{"۱۲۳", 0},                  // 非 ASCII 数字不给它猜
	}
	for _, c := range cases {
		if got := atoiSafe(c.in); got != c.want {
			t.Errorf("atoiSafe(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestTrimUnicodeSpace 全角空格等 Unicode 空白都要去掉。
//
// ⚠️ 原先这个函数的注释写着"用 strings.TrimSpace 去不掉全角空格"，是**错的**：
// strings.TrimSpace 内部就是 TrimFunc(s, unicode.IsSpace)，而 unicode.IsSpace
// 对 U+3000 返回 true（2026-09-21 实测）。这条测试把真实行为钉住，
// 免得又有人照着旧注释去"加强"它。
func TestTrimUnicodeSpace(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"\u3000姐姐\u3000", "姐姐"}, // 全角空格 U+3000
		{"\u00a0x\u00a0", "x"},   // NBSP
		{"\u2003x\u2003", "x"},   // EM SPACE
		{"  x  ", "x"},
		{"\t\nx\r", "x"},
		{"x", "x"},
		{"", ""},
		{"\u3000", ""}, // 全是空白 → 空串
		// U+200B（零宽空格）**不是** White_Space，故意保留 —— 它可能是有意义的
		// 排版字符，去掉会改变文本宽度（inline_text 的换行计算依赖真实宽度）。
		{"\u200bx", "\u200bx"},
	}
	for _, c := range cases {
		if got := trimUnicodeSpace(c.in); got != c.want {
			t.Errorf("trimUnicodeSpace(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 与 strings.TrimSpace 等价，这个事实值得显式写下来。
	for _, s := range []string{"\u3000x\u3000", "\u00a0y", " z ", "\u2003\u2003"} {
		if got, want := trimUnicodeSpace(s), strings.TrimSpace(s); got != want {
			t.Errorf("trimUnicodeSpace(%q) = %q，而 strings.TrimSpace = %q —— 两者应当一致", s, got, want)
		}
	}
}

// TestEnsureExt 扩展名大小写不敏感，缺了才补。
func TestEnsureExt(t *testing.T) {
	cases := []struct {
		path, ext, want string
	}{
		{"a.jpg", ".jpg", "a.jpg"},
		{"a.JPG", ".jpg", "a.JPG"}, // 大小写不敏感，不重复追加
		{"a.webp", ".jpg", "a.webp.jpg"},
		{"a", ".jpg", "a.jpg"},
		{"dir/a", ".png", "dir/a.png"},
		// 注意：filepath.Ext("a.") 是 "."，与原扩展名 .jpg 不同 → 会追加。
		{"a.", ".jpg", "a..jpg"},
	}
	for _, c := range cases {
		if got := ensureExt(c.path, c.ext); got != c.want {
			t.Errorf("ensureExt(%q, %q) = %q，期望 %q", c.path, c.ext, got, c.want)
		}
	}
}

// TestIsProbablyHTMLTitle 只认整页 HTML 的标记。
//
// 它挡的是"CDN 出错返回错误页、上游把页面标题当文件名"这种情况，
// 所以只认 <!doctype / <html 这种整页标记；普通尖括号文本不该被误判
// （否则含 "<3" 之类的标题会被整条丢掉）。
func TestIsProbablyHTMLTitle(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"<!DOCTYPE html><html>...", true},
		{"<!doctype html>", true},
		{"<html><head></head></html>", true},
		{"<HTML>", true}, // 大小写不敏感
		{"<p>片段</p>", false},
		{"我叫<小明>", false},
		{"普通标题", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isProbablyHTMLTitle(c.in); got != c.want {
			t.Errorf("isProbablyHTMLTitle(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// TestChapterNames 章节名列表：空入参给 nil，顺序原样。
func TestChapterNames(t *testing.T) {
	if got := chapterNames(nil); got != nil {
		t.Errorf("无章节应返回 nil，实际 %v", got)
	}
	if got := chapterNames([]ChapterBrief{}); got != nil {
		t.Errorf("空切片应返回 nil，实际 %v", got)
	}

	chs := []ChapterBrief{{ID: 3, Name: "第三章"}, {ID: 1, Name: "第一章"}, {ID: 2, Name: "第二章"}}
	got := chapterNames(chs)
	want := []string{"第三章", "第一章", "第二章"}
	if len(got) != len(want) {
		t.Fatalf("chapterNames = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %q，期望 %q（顺序原样，不排序）", i, got[i], want[i])
		}
	}
}

// readingIndex 的基础用例在 service_test.go:TestReadingIndex 里。
// 这里不再重复定义 —— 只在那边补了 nil / 负数两个边界。
