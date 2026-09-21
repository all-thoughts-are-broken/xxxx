package client

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubTransport 记录收到的请求，替代真实网络。
//
// 之所以必须在这一层断言：SignatureRoundTripper 的产物**只在链路最底层**才可见。
// 任何套在它外面的东西（包括测试里顺手包一层的 Transport）看到的都是签名前的
// 请求，于是"头到底对不对"永远测不到。
type stubTransport struct {
	headers http.Header
	url     string
	calls   int
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	s.headers = req.Header.Clone()
	s.url = req.URL.String()
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"code":200,"data":"x"}`)),
		Request:    req,
	}, nil
}

func md5HexForTest(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestSignatureHeadersAreNotSwapped 是本文件里最要紧的一条回归测试。
//
// 背景：tokenAndTokenParam 刻意把返回顺序固定成 (token, tokenParam)，
// 目的就是防住历史上那个 (tokenparam, token) 的易错顺序。但唯一调用点偏偏
// 按错顺序接收，导致上线时两个头的值互换 —— 服务端在 tokenparam 里读不到
// 时间戳，改用自己时钟推导响应密钥，客户端表现出的却是
// "解密响应失败: 去除填充失败: 非法的填充长度 N"，看起来像加解密算法错了。
//
// 断言用的是真实抓包值，所以它同时锁住了：头名、头值、以及 token 的算法。
func TestSignatureHeadersAreNotSwapped(t *testing.T) {
	const (
		ts        = "1752484996"
		ver       = "1.8.0"
		secret    = "185Hcomic3PAPP7R"
		wantToken = "3ffb5bcfa3f94c10509b02d1aec20634" // 真实抓包
	)

	stub := &stubTransport{}
	srt := NewSignatureRoundTripper(stub, ver, secret, "TEST-UA")

	req, err := http.NewRequestWithContext(
		WithTimestamp(context.Background(), ts),
		http.MethodGet, "https://example.com/comic_read?id=1", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := srt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}

	gotToken := stub.headers.Get("token")
	gotParam := stub.headers.Get("tokenparam")

	if gotToken != wantToken {
		t.Errorf("token = %q, 期望 %q", gotToken, wantToken)
	}
	if gotParam != ts+","+ver {
		t.Errorf("tokenparam = %q, 期望 %q", gotParam, ts+","+ver)
	}

	// 单独再判一次"有没有被对调"：上面两条已经覆盖，但这条的报错信息
	// 直接点出病因，将来若有人重构签名的返回顺序，失败原因一眼可见。
	if gotToken == ts+","+ver || gotParam == wantToken {
		t.Fatalf("token / tokenparam 被对调了：token=%q tokenparam=%q（"+
			"检查 tokenAndTokenParam 的接收顺序）", gotToken, gotParam)
	}

	// tokenparam 必须是"时间戳,版本"，也就是含逗号、且描述是数字。
	if !strings.Contains(gotParam, ",") || !strings.HasPrefix(gotParam, ts) {
		t.Errorf("tokenparam 格式不对: %q（期望 <timestamp>,<version>）", gotParam)
	}
	// token 必须是 32 位十六进制（md5）。
	if len(gotToken) != 32 {
		t.Errorf("token 长度 = %d，期望 32（md5 十六进制）", len(gotToken))
	}
}

// TestSignatureTokenMatchesTimestampAndSecret 在"没有注入时间戳"时，
// 反推出签名用的时间戳，再验证 token 与该时间戳自洽。
//
// 这样即使没有真实抓包，也能卡住"两头用了不同的时间戳"这类错误。
func TestSignatureTokenMatchesTimestampAndSecret(t *testing.T) {
	const (
		ver    = "1.8.0"
		secret = "185Hcomic3PAPP7R"
	)

	stub := &stubTransport{}
	srt := NewSignatureRoundTripper(stub, ver, secret, "TEST-UA")

	// 故意不注入时间戳，让 RoundTripper 自己取 Now()
	req, err := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}

	param := stub.headers.Get("tokenparam")
	i := strings.Index(param, ",")
	if i <= 0 {
		t.Fatalf("tokenparam 里没有时间戳: %q", param)
	}
	tsFromHeader := param[:i]
	if param[i+1:] != ver {
		t.Errorf("tokenparam 的版本 = %q, 期望 %q", param[i+1:], ver)
	}

	if want := md5HexForTest(tsFromHeader + secret); stub.headers.Get("token") != want {
		t.Errorf("token = %q, 期望 md5(%s+secret) = %q",
			stub.headers.Get("token"), tsFromHeader, want)
	}
}

// TestSignatureInjectsTimestampIntoContext 确认签名用的时间戳被回写进 context。
//
// doGet 依赖这一点：它先用自己生成的时间戳解密，若 RoundTripper 换了另一个
// 值（比如重新取 Now()），跨过秒边界就会解密失败 —— 一个极难复现的偶发 bug。
func TestSignatureInjectsTimestampIntoContext(t *testing.T) {
	const injected = "1752484996"

	var fromCtx string
	var ok bool
	stub := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		fromCtx, ok = TimestampFrom(req.Context())
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	})

	srt := NewSignatureRoundTripper(stub, "1.8.0", "185Hcomic3PAPP7R", "UA")
	req, err := http.NewRequestWithContext(
		WithTimestamp(context.Background(), injected),
		http.MethodGet, "https://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}

	if !ok || fromCtx != injected {
		t.Errorf("context 里的时间戳 = %q(ok=%v), 期望 %q", fromCtx, ok, injected)
	}
}

// TestSignatureDoesNotMutateCallerRequest 确认签名不改调用方持有的请求对象。
//
// 同一个 *http.Request 被并发复用时改它会踩数据竞争；所以实现里先 Clone。
func TestSignatureDoesNotMutateCallerRequest(t *testing.T) {
	stub := &stubTransport{}
	srt := NewSignatureRoundTripper(stub, "1.8.0", "185Hcomic3PAPP7R", "UA")

	req, err := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}

	if got := req.Header.Get("token"); got != "" {
		t.Errorf("调用方的请求被写入了 token=%q（应当 Clone 后再改）", got)
	}
	if got := req.Header.Get("tokenparam"); got != "" {
		t.Errorf("调用方的请求被写入了 tokenparam=%q（应当 Clone 后再改）", got)
	}
}

// roundTripperFunc 让函数能直接当 RoundTripper 用。
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
