package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type contextKey string

const (
	timestampKey contextKey = "jm_req_timestamp"
)

// SignatureRoundTripper 负责在请求发送前自动计算签名并注入认证头。
type SignatureRoundTripper struct {
	next      http.RoundTripper
	version   string
	secret    string
	userAgent string
}

// NewSignatureRoundTripper 创建一个新的签名 RoundTripper。
func NewSignatureRoundTripper(next http.RoundTripper, version, secret, userAgent string) *SignatureRoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	return &SignatureRoundTripper{
		next:      next,
		version:   version,
		secret:    secret,
		userAgent: userAgent,
	}
}

const defaultUserAgent = "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/116.0.0.0 Mobile Safari/537.36"

// RoundTrip 实现 http.RoundTripper。
//
// 时间戳的来源优先级：请求 context（由 WithTimestamp 注入）> 当前时间。
//
// 这里做了相对原实现的关键修正：时间戳**必须**由发起请求的一方决定，并把同一个
// 值用于签名和响应解密。原实现让 RoundTripper 自己取 Now()，解密端再从
// resp.Request.Context() 里把它捞回来——虽然 net/http 确实会把原始请求
// 挂回 resp.Request（transport.go 的 `resp.Request = origReq`），链路是通的，
// 但一旦跨过一秒边界或中间被重定向，解密就会以一个语焉不详的
// "去除填充失败" 报错。现在由 doGet 显式生成并传递，把这条隐式依赖去掉。
func (srt *SignatureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ts, ok := TimestampFrom(req.Context())
	if !ok {
		ts = strconv.FormatInt(time.Now().Unix(), 10)
	}

	// 注意这里的接收顺序：tokenAndTokenParam 刻意把顺序固定成
	// (token, tokenParam) —— 反过来的 (tokenparam, token) 是历史签名顺序，
	// 极易写反。写反的后果不是报错，而是两个头互换值：服务端在 tokenparam
	// 里读不到时间戳，就用它自己的时钟推导响应密钥，客户端于是怎么解都
	// 得到一个语焉不详的「去除填充失败: 非法的填充长度」。
	token, tokenParam := tokenAndTokenParam(ts, srt.version, srt.secret)

	// 克隆请求，避免修改调用方持有的对象（并发复用同一个 *http.Request 会踩数据竞争）。
	req = req.Clone(req.Context())
	req.Header.Set("token", token)
	req.Header.Set("tokenparam", tokenParam)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", srt.userAgent)
	}

	ctx := WithTimestamp(req.Context(), ts)
	req = req.WithContext(ctx)

	return srt.next.RoundTrip(req)
}

// WithTimestamp 把签名用的时间戳注入 context。
func WithTimestamp(ctx context.Context, ts string) context.Context {
	return context.WithValue(ctx, timestampKey, ts)
}

// TimestampFrom 从 context 取出签名用的时间戳。
func TimestampFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(timestampKey).(string)
	return v, ok
}

// GetRequestTimestamp 保留旧名，等价于 TimestampFrom。
//
// Deprecated: 用 TimestampFrom。
func GetRequestTimestamp(ctx context.Context) (string, bool) { return TimestampFrom(ctx) }

// ---- HTTP 错误包装 ----

// NetworkError 表示请求根本没拿到响应（DNS / 连接 / TLS / 超时）。
type NetworkError struct {
	URL string
	Err error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("请求 %s 失败: %v", e.URL, e.Err)
}

func (e *NetworkError) Unwrap() error { return e.Err }

// StatusError 表示拿到了响应但状态码不合法。
type StatusError struct {
	URL        string
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("请求 %s 返回异常状态码 %d", e.URL, e.StatusCode)
	}
	return fmt.Sprintf("请求 %s 返回异常状态码 %d: %s", e.URL, e.StatusCode, e.Body)
}

// IsNetworkError 报告错误是否属于网络层问题。
func IsNetworkError(err error) bool {
	var ne *NetworkError
	return errors.As(err, &ne)
}
