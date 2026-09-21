package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- 页码补零 ----

// TestPadPage 锁住「URL 里的页码是 5 位、补前导 0」这条规则。
//
// ⚠️ 这条与 utils.bookmarkName（PDF 书签）的规则**方向相反**，别当成同一个东西
// 去"统一"：
//
//	URL 文件名 /media/photos/<aid>/00011.webp  ← 服务端要求，必须补零
//	阅读器里的书签标题  第 11 页               ← 给人看，要去零（bookmarkName）
//
// utils/bookmarkName 已有测试（pdf_test.go）；这条是它的镜像，两边都锁住，
// 将来谁想"顺手统一"，至少会有一条变红。
func TestPadPage(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{1, "00001"},
		{9, "00009"},
		{10, "00010"},
		{99, "00099"},
		{100, "00100"},
		{9999, "09999"},
		{10000, "10000"}, // 恰好 5 位，原样
		{99999, "99999"},
		{100000, "100000"}, // 超过 5 位**不截断** —— 截断会取到别人的页
	}
	for _, c := range cases {
		if got := PadPage(c.in); got != c.want {
			t.Errorf("PadPage(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ---- cdnBase：host 归一化 ----

// TestCdnBaseNeverDoublesScheme 三种 host 写法都要归一成 "https://host"。
//
// 这是为了防 "https://https://cdn..." 这种坏 URL —— 它不会立刻报错，
// 只会让图片全部下载失败，排查起来像是 CDN 挂了。
func TestCdnBaseNeverDoublesScheme(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"cdn-msp3.example.cc", "https://cdn-msp3.example.cc"},
		{"https://cdn-msp3.example.cc", "https://cdn-msp3.example.cc"},
		{"http://cdn-msp3.example.cc", "http://cdn-msp3.example.cc"},
		{"cdn-msp3.example.cc/", "https://cdn-msp3.example.cc"},
		{"https://cdn-msp3.example.cc/", "https://cdn-msp3.example.cc"},
		{"  cdn-msp3.example.cc  ", "https://cdn-msp3.example.cc"},
	}
	for _, c := range cases {
		got := cdnBase(c.in)
		if got != c.want {
			t.Errorf("cdnBase(%q) = %q，期望 %q", c.in, got, c.want)
		}
		if strings.Count(got, "://") > 1 {
			t.Errorf("cdnBase(%q) = %q —— 拼出了重复协议头", c.in, got)
		}
	}
}

// ---- 三类图片 URL 的路径规则各不同 ----

// TestBuildAlbumCoverURL 封面固定是 /media/albums/<aid>_3x4.jpg。
func TestBuildAlbumCoverURL(t *testing.T) {
	got := BuildAlbumCoverURL(1423323, "cdn-msp3.jmapiproxy1.cc")
	want := "https://cdn-msp3.jmapiproxy1.cc/media/albums/1423323_3x4.jpg"
	if got != want {
		t.Errorf("got %q，期望 %q", got, want)
	}

	// host 缺失时必须返回空串，让调用方降级成「无封面」，
	// 而不是拼出一个指向空 host 的坏 URL 去白白下载一轮。
	if got := BuildAlbumCoverURL(1423323, ""); got != "" {
		t.Errorf("host 为空应返回空串，实际 %q", got)
	}
	if got := BuildAlbumCoverURL(1423323, "   "); got != "" {
		t.Errorf("host 全是空白应返回空串，实际 %q", got)
	}
	// aid <= 0 是无效作品号。
	if got := BuildAlbumCoverURL(0, "cdn.cc"); got != "" {
		t.Errorf("aid=0 应返回空串，实际 %q", got)
	}
	if got := BuildAlbumCoverURL(-1, "cdn.cc"); got != "" {
		t.Errorf("aid<0 应返回空串，实际 %q", got)
	}
}

// TestBuildUserAvatarURL 头像要补 /media/users/ 前缀，且容忍 photo 带前导斜杠。
func TestBuildUserAvatarURL(t *testing.T) {
	host := "cdn-msp.jmapiproxy3.cc"

	if got, want := BuildUserAvatarURL(host, "nopic-Male.gif"),
		"https://cdn-msp.jmapiproxy3.cc/media/users/nopic-Male.gif"; got != want {
		t.Errorf("got %q，期望 %q", got, want)
	}

	// 接口有时给 "/xxx.gif"，TrimLeft 掉前导斜杠避免出 "media//users"。
	got := BuildUserAvatarURL(host, "/nopic-Male.gif")
	if strings.Contains(got, "//media") || strings.Contains(got, "users//") {
		t.Errorf("photo 带前导斜杠时拼出了双斜杠: %q", got)
	}
	if want := "https://cdn-msp.jmapiproxy3.cc/media/users/nopic-Male.gif"; got != want {
		t.Errorf("got %q，期望 %q", got, want)
	}

	// photo 已经是完整 URL 时**原样返回**，不能再去拼 host。
	full := "https://www.cdnbea.net/media/users/x.gif"
	if got := BuildUserAvatarURL(host, full); got != full {
		t.Errorf("完整 URL 应原样返回，got %q", got)
	}
	if got := BuildUserAvatarURL(host, "http://x.cc/a.gif"); got != "http://x.cc/a.gif" {
		t.Errorf("http 开头的完整 URL 应原样返回，got %q", got)
	}

	// photo 为空 → 空串（调用方降级），host 为空 → 空串。
	if got := BuildUserAvatarURL(host, ""); got != "" {
		t.Errorf("photo 为空应返回空串，实际 %q", got)
	}
	if got := BuildUserAvatarURL("", "nopic-Male.gif"); got != "" {
		t.Errorf("host 为空应返回空串，实际 %q", got)
	}
}

// TestBuildBadgeImageURL 勋章**不补任何路径前缀**（photo 已是完整路径）。
//
// 这是三类 URL 里最容易改错的一个：cover 固定补 /media/albums/、avatar 补
// /media/users/，而 badge 的 photo 本身就是 "/static/resources/images/..."，
// 再加前缀会 404。把它和 avatar 的差异锁住。
func TestBuildBadgeImageURL(t *testing.T) {
	host := "cdn-msp.jmapiproxy3.cc"
	photo := "/static/resources/images/勋章/2021.8勋章/maidragon_8.png"
	want := "https://cdn-msp.jmapiproxy3.cc/static/resources/images/勋章/2021.8勋章/maidragon_8.png"

	if got := BuildBadgeImageURL(host, photo); got != want {
		t.Errorf("got %q，期望 %q", got, want)
	}
	// 显式断言「没有多补前缀」—— 这是这条测试存在的理由。
	if got := BuildBadgeImageURL(host, photo); strings.Contains(got, "/media/users/") ||
		strings.Contains(got, "/media/albums/") {
		t.Errorf("勋章 URL 被多补了路径前缀: %q", got)
	}
	// 无前导斜杠的 photo 也要能拼对（不能出 "https://hoststatic/..."）。
	if got, want := BuildBadgeImageURL(host, "static/x.png"),
		"https://cdn-msp.jmapiproxy3.cc/static/x.png"; got != want {
		t.Errorf("got %q，期望 %q", got, want)
	}

	full := "https://other.cc/badge.png"
	if got := BuildBadgeImageURL(host, full); got != full {
		t.Errorf("完整 URL 应原样返回，got %q", got)
	}
	if got := BuildBadgeImageURL(host, ""); got != "" {
		t.Errorf("photo 为空应返回空串，实际 %q", got)
	}
	if got := BuildBadgeImageURL("", "/static/x.png"); got != "" {
		t.Errorf("host 为空应返回空串，实际 %q", got)
	}
}

// TestBuildReadImageURL 阅读图 URL：/media/photos/<aid>/<5 位页码>.webp。
func TestBuildReadImageURL(t *testing.T) {
	got := BuildReadImageURL(1423323, 11, "cdn-msp.jmapiproxy3.cc")
	want := "https://cdn-msp.jmapiproxy3.cc/media/photos/1423323/00011.webp"
	if got != want {
		t.Errorf("got %q，期望 %q", got, want)
	}

	// 非法入参一律空串，别拼半个 URL 出去。
	for _, c := range []struct {
		aid, page int
		host      string
		why       string
	}{
		{0, 1, "cdn.cc", "aid=0"},
		{1, 0, "cdn.cc", "page=0"},
		{1, -1, "cdn.cc", "page<0"},
		{1, 1, "", "host 为空"},
		{1, 1, "   ", "host 全空白"},
	} {
		if got := BuildReadImageURL(c.aid, c.page, c.host); got != "" {
			t.Errorf("%s 应返回空串，实际 %q", c.why, got)
		}
	}
}

// ---- cleanCandidates ----

// TestCleanCandidates 去空白、去尾斜杠、去重，且**保持原顺序**。
//
// 顺序有意义：ProbeBaseURLs 是「按顺序试、命中即停」，重排会改掉优先级。
func TestCleanCandidates(t *testing.T) {
	in := []string{
		"  a.cc  ",
		"b.cc/",
		"a.cc",   // 与第一条去空白后重复
		"",       // 空串丢弃
		"   ",    // 全空白丢弃
		"/",      // 只剩斜杠 → trim 后空 → 丢弃
		"c.cc//", // 尾斜杠全去掉
		"b.cc",   // 重复
	}
	got := cleanCandidates(in)
	want := []string{"a.cc", "b.cc", "c.cc"}

	if len(got) != len(want) {
		t.Fatalf("cleanCandidates = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %q，期望 %q（顺序也要保持）", i, got[i], want[i])
		}
	}

	// 全空的输入要返回**空切片而不是 nil** —— 上层会直接拿它当候选表遍历。
	if got := cleanCandidates(nil); got == nil {
		t.Error("空输入应返回空切片而非 nil")
	}
	if got := cleanCandidates([]string{"", "  "}); len(got) != 0 {
		t.Errorf("全是无效项时应返回空表，实际 %v", got)
	}
}

// ---- ProbeResult 的汇总 ----

// TestProbeResultOKAndError 全失败时要把每条线路的原因汇总成人话。
//
// 宿主看到的是这一句话，它得能回答"为什么探不到线路"——
// 只说"失败"就等于把排查成本推给了用户。
func TestProbeResultOKAndError(t *testing.T) {
	ok := ProbeResult{BaseURL: "https://a.cc", CDNHost: "cdn.cc"}
	if !ok.OK() {
		t.Error("有 BaseURL 时 OK() 应为 true")
	}
	if s := ok.Error(); s != "" {
		t.Errorf("成功时 Error() 应为空串，实际 %q", s)
	}

	bad := ProbeResult{Attempts: []ProbeAttempt{
		{BaseURL: "https://a.cc", LatencyMS: 1200, Err: errors.New("超时")},
		{BaseURL: "https://b.cc", LatencyMS: 30, Err: errors.New("连接被拒")},
		{BaseURL: "https://c.cc", LatencyMS: 45, Err: nil}, // 这条成功但没被选中（理论不会出现）
	}}
	if bad.OK() {
		t.Error("BaseURL 为空时 OK() 应为 false")
	}
	msg := bad.Error()
	for _, want := range []string{"所有候选线路都不可用", "https://a.cc", "超时", "https://b.cc", "连接被拒"} {
		if !strings.Contains(msg, want) {
			t.Errorf("汇总信息缺少 %q，实际: %s", want, msg)
		}
	}
	if strings.Contains(msg, "https://c.cc") {
		t.Errorf("成功的那条不该出现在失败汇总里，实际: %s", msg)
	}
}

// ---- 探活：状态切换与命中即停 ----

// readPageEnvelope 造一个「能让探活判定成功」的响应体。
//
// 故意走 Envelope.payload 的**明文兜底**分支（data 直接是 JSON 对象，不是 Base64
// 密文）：这样测试不必真的走一遍加解密，也就不会因为密钥或时间戳变化而脆断。
// 兜底分支本身是 client.go 里写明的公开行为，用它造数据是合理的。
func readPageEnvelope(imageURL string) string {
	body, err := json.Marshal(map[string]any{
		"code": 200,
		"data": map[string]any{
			"id":     1472136,
			"name":   "探活样例",
			"images": []map[string]string{{"image": imageURL}},
		},
	})
	if err != nil {
		panic(err) // 常量结构的序列化不会失败
	}
	return string(body)
}

// newProbeServer 起一个用**路径前缀**模拟多条线路的服务端。
//
//	/ok/...   → 立即可探通（图片直链的 host 是 cdn-ok.cc）
//	/slow/... → 延迟 120ms 后可探通（host 是 cdn-slow.cc）
//	其它      → 500
//
// 用路径区分而不是域名，是因为候选线路要拼成 `baseURL + "/comic_read"`，
// 同一台测试服务器只能靠路径分辨调用方试的是哪一条。
func newProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ok/"):
			_, _ = w.Write([]byte(readPageEnvelope("https://cdn-ok.cc/media/photos/1472136/00001.webp")))
		case strings.HasPrefix(r.URL.Path, "/slow/"):
			time.Sleep(120 * time.Millisecond)
			_, _ = w.Write([]byte(readPageEnvelope("https://cdn-slow.cc/media/photos/1472136/00001.webp")))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newProbeClient 造一个 HTTP 客户端已指向测试服务器的 client。
//
// WithHTTPClient 注入的 client **不会**被 NewClient 覆盖（见 `if c.httpClient == nil`），
// 代价是签名头不会自动添加 —— 但这里返回的是明文信封，本来也不需要签名。
func newProbeClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(
		WithHTTPClient(srv.Client()),
		WithSecret("test-secret"),
		WithAppVersion("1.0.0"),
	)
	if err != nil {
		t.Fatalf("NewClient 失败: %v", err)
	}
	return c
}

// TestProbeBaseURLsSwitchesToFirstWorkingLine 探到可用线路后，要把 client 的
// base_url 与 cdn_host **就地切过去**，并顺带从图片直链里学出 CDN 域名。
//
// 这条测试是必要的：原来的版本只断言「全部失败时状态不变」，而失败路径下
// ProbeBaseURLs 根本不会调 SetBaseURL（它只在成功后 set），所以那条断言
// **永远成立、什么也没验证** —— 把还原代码删掉它也不红。改成从成功路径切进去。
func TestProbeBaseURLsSwitchesToFirstWorkingLine(t *testing.T) {
	srv := newProbeServer(t)
	c := newProbeClient(t, srv)

	fail := srv.URL + "/fail"
	good := srv.URL + "/ok"

	res := c.ProbeBaseURLs(context.Background(), []string{fail, good}, 1472136)

	if !res.OK() {
		t.Fatalf("应当探到可用线路，实际 %+v", res)
	}
	if res.BaseURL != good {
		t.Errorf("res.BaseURL = %q，期望 %q", res.BaseURL, good)
	}
	if res.CDNHost != "cdn-ok.cc" {
		t.Errorf("res.CDNHost = %q，期望从图片直链里学出 cdn-ok.cc", res.CDNHost)
	}
	// 状态要真的切过去，否则调用方拿到 res 还得自己再 set 一遍。
	if got := c.BaseURL(); got != good {
		t.Errorf("探活成功后 client.base_url = %q，期望 %q", got, good)
	}
	if got := c.CDNHost(); got != "cdn-ok.cc" {
		t.Errorf("探活成功后 client.cdn_host = %q，期望 cdn-ok.cc", got)
	}
	// 第一条失败、第二条成功，所以 Attempts 应当恰好两条，第一条带错误。
	if len(res.Attempts) != 2 {
		t.Fatalf("Attempts 应有 2 条，实际 %d 条: %+v", len(res.Attempts), res.Attempts)
	}
	if res.Attempts[0].Err == nil {
		t.Error("第一条线路（500）应当记录了错误")
	}
	if res.Attempts[1].Err != nil {
		t.Errorf("第二条线路应当成功，实际错误: %v", res.Attempts[1].Err)
	}
}

// TestProbeBaseURLsStopsAtFirstSuccess 命中即停。
//
// 「顺序」是这个函数存在的意义（兜底列表里掺着失效线路，先试优先级高的），
// 如果它把候选全部试一遍，就退化成了慢且不必要的全量探测。
func TestProbeBaseURLsStopsAtFirstSuccess(t *testing.T) {
	srv := newProbeServer(t)
	c := newProbeClient(t, srv)

	good := srv.URL + "/ok"

	res := c.ProbeBaseURLs(context.Background(), []string{good, srv.URL + "/fail"}, 1472136)

	if !res.OK() {
		t.Fatalf("应当探到可用线路，实际 %+v", res)
	}
	if len(res.Attempts) != 1 {
		t.Errorf("第一条就成功了，不该再试后面的：Attempts = %+v", res.Attempts)
	}
}

// TestProbeBaseURLsKeepsStateWhenAllFail 全部失败时保持探活前的状态。
//
// ⚠️ 注意这条测试**锁的是契约，不是实现细节**：当前实现只在成功时才
// SetBaseURL，失败路径压根没碰过状态，所以那两行「还原」代码实际上跑不出差异
// （删掉它们这条测试照样绿）。留它是为了挡住将来有人改成「先 set 再验证」——
// 那时这条测试会立刻变红，提醒他补上还原。
func TestProbeBaseURLsKeepsStateWhenAllFail(t *testing.T) {
	srv := newProbeServer(t)
	c := newProbeClient(t, srv)

	c.SetBaseURL("https://keep-me.cc")
	c.SetCDNHost("keep-cdn.cc")

	res := c.ProbeBaseURLs(context.Background(), []string{srv.URL + "/fail", srv.URL + "/fail2"}, 1472136)

	if res.OK() {
		t.Fatalf("这些线路都返回 500，不该探通，实际探到 %q", res.BaseURL)
	}
	if got := c.BaseURL(); got != "https://keep-me.cc" {
		t.Errorf("失败后 base_url 被改坏了: %q", got)
	}
	if got := c.CDNHost(); got != "keep-cdn.cc" {
		t.Errorf("失败后 cdn_host 被改坏了: %q", got)
	}
	if res.Error() == "" {
		t.Error("全失败时 Error() 要给出汇总原因")
	}
}

// TestProbeFastestPicksLowestLatency 并发探活要选**延迟最低**的那条。
//
// 为什么值得为它写测试：线路之间延迟能差一倍以上，而下载一本要打几百次接口，
// 选错一条会被放大成实打实的时间。这里造一条 120ms 与一条即时的线路，
// 断言赢家是即时那条（顺带覆盖 single-flight 之外的并发分支）。
func TestProbeFastestPicksLowestLatency(t *testing.T) {
	srv := newProbeServer(t)
	c := newProbeClient(t, srv)

	slow := srv.URL + "/slow"
	fast := srv.URL + "/ok"

	// 故意把慢的放前面：结果必须是「最快」而不是「第一个」。
	res := c.ProbeFastest(context.Background(), []string{slow, fast}, 1472136, 4)

	if !res.OK() {
		t.Fatalf("应当探到可用线路，实际 %+v", res)
	}
	if res.BaseURL != fast {
		t.Errorf("赢家 = %q，期望延迟最低的 %q", res.BaseURL, fast)
	}
	if got := c.BaseURL(); got != fast {
		t.Errorf("client.base_url = %q，期望切到赢家 %q", got, fast)
	}
	// 两条都要有记录（并发全试），且慢的那条耗时明显更大。
	if len(res.Attempts) != 2 {
		t.Fatalf("Attempts 应有 2 条，实际 %d 条", len(res.Attempts))
	}
	var slowMS int64
	for _, a := range res.Attempts {
		if a.BaseURL == slow {
			slowMS = a.LatencyMS
		}
	}
	if slowMS < 100 {
		t.Errorf("慢线路记录的耗时 = %dms，明显偏小（sleep 的是 120ms）", slowMS)
	}
}

// ---- ReadPageResult 的小工具 ----

// TestReadPageResultScrambleID scramble_id 是字符串，非数字要退成 0。
//
// 退成 0 的后果是「这张图不还原」——会安静地输出花屏，所以这里的边界
// （空串、负数、非数字、带空白）都值得锁住。
func TestReadPageResultScrambleID(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"220980", 220980},
		{" 123 ", 123},
		{"0", 0},   // 0 表示不需要还原
		{"", 0},    // 缺字段
		{"abc", 0}, // 非数字
		{"-5", 0},  // 负数非法
		{"12.5", 0},
	}
	for _, c := range cases {
		r := &ReadPageResult{ScrambleId: c.in}
		if got := r.ScrambleID(); got != c.want {
			t.Errorf("ScrambleID(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestReadPageResultImageURLs 过滤空直链，但**保持原顺序**。
//
// 顺序就是页码顺序：错一个，后面所有页都会前移（与真实章节错位）。
// 空串也必须滤掉 —— 否则会变成对 base_url 的一次无意义请求。
func TestReadPageResultImageURLs(t *testing.T) {
	r := &ReadPageResult{Images: []Images{
		{Image: "https://cdn.cc/1.webp"},
		{Image: ""},
		{Image: "   "},
		{Image: "https://cdn.cc/4.webp?t=1"},
	}}
	got := r.ImageURLs()
	want := []string{"https://cdn.cc/1.webp", "https://cdn.cc/4.webp?t=1"}

	if len(got) != len(want) {
		t.Fatalf("ImageURLs = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %q，期望 %q（顺序即页码）", i, got[i], want[i])
		}
	}

	if empty := (&ReadPageResult{}).ImageURLs(); len(empty) != 0 {
		t.Errorf("没有图片时应返回空表，实际 %v", empty)
	}
}
