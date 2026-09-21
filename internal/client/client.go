// Package client 封装 JM 服务端接口：签名头、AES 解密、各 endpoint 的请求与解析。
//
// 它 import protocol，但**只把它当错误码词汇表**用：在语义明确的场合
// （如服务端用 `[]` 表示对象不存在，见 README 6.17）直接给出 CodeNotFound，
// 免得上层把"这本不存在"误报成 internal。它不碰帧、Server、Task，
// 也不认识 JSON 行协议。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// DefaultTimeout 是单次接口请求的默认超时。
const DefaultTimeout = 30 * time.Second

// Client 是 JM API 客户端。
type Client struct {
	mu         sync.RWMutex
	baseURL    string
	version    string
	secret     string
	proxy      string
	timeout    time.Duration
	userAgent  string
	cdnHost    string
	httpClient *http.Client
}

// Option 配置客户端选项的函数。
type Option func(*Client)

// WithBaseURL 设置 API 请求的基础 URL。
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		c.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
}

// WithAppVersion 设置客户端 APP 版本号。
func WithAppVersion(version string) Option {
	return func(c *Client) {
		c.version = version
	}
}

// WithSecret 设置签名与响应解密共用的密钥。
//
// 已用真实抓包核对：token 与响应解密用的是同一个 secret，
// 例如 md5("1752484996"+"185Hcomic3PAPP7R") 就等于抓包里的 token。
func WithSecret(secret string) Option {
	return func(c *Client) {
		c.secret = secret
	}
}

// WithProxy 设置 HTTP / HTTPS / SOCKS5 代理。
func WithProxy(proxy string) Option {
	return func(c *Client) {
		c.proxy = proxy
	}
}

// WithTimeout 设置请求超时时间。
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		c.timeout = timeout
	}
}

// WithUserAgent 设置自定义 User-Agent。
func WithUserAgent(userAgent string) Option {
	return func(c *Client) {
		c.userAgent = userAgent
	}
}

// WithCDNHost 直接指定图片 CDN 域名，跳过线路探测。
func WithCDNHost(host string) Option {
	return func(c *Client) {
		c.cdnHost = strings.TrimSpace(host)
	}
}

// WithHTTPClient 允许外部注入自定义的 http.Client。
//
// 注意：注入后签名头不会被自动添加，除非调用方自己把
// NewSignatureRoundTripper 接进 Transport 链。
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// NewClient 创建并初始化一个 JM API 客户端。
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{
		timeout: DefaultTimeout,
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.secret == "" {
		return nil, fmt.Errorf("secret 不能为空：没有它既算不出签名头，也解不开响应")
	}

	// baseURL 允许留空：此时调用方应先通过 ProbeBaseURLs 探活再 WithBaseURL。
	// 这里不报错，是为了让「先建 client 再探线路」这种用法成为可能。

	if c.httpClient == nil {
		baseTransport := http.DefaultTransport.(*http.Transport).Clone()

		if c.proxy != "" {
			proxyURL, err := url.Parse(c.proxy)
			if err != nil {
				return nil, fmt.Errorf("代理地址 %q 非法: %w", c.proxy, err)
			}
			baseTransport.Proxy = http.ProxyURL(proxyURL)
		}

		c.httpClient = &http.Client{
			Transport: NewSignatureRoundTripper(baseTransport, c.version, c.secret, c.userAgent),
			Timeout:   c.timeout,
			// 不跟随重定向：签名是通过**自定义头** token / tokenparam 发出去的，
			// 而 net/http 只在跨域重定向时剥掉 Authorization / Cookie，
			// **不会**动自定义头 —— 线路一旦被劫持或域名过期跳到别处，
			// 签名（含时间戳）就跟着发给对方了。
			//
			// 实测正常线路与图片 CDN 都不返回 302（0 次重定向），所以不跟随
			// 不影响现有功能；真遇到跳转，让调用方拿到 3xx 当作状态码错误
			// 更安全，也比闷声跟着走更好排查。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return c, nil
}

// BaseURL 获取当前配置的 BaseURL。
func (c *Client) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// SetBaseURL 在运行期切换 API 线路（线路失效时换线用）。
func (c *Client) SetBaseURL(baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// CDNHost 返回图片 CDN 域名。
func (c *Client) CDNHost() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cdnHost
}

// SetCDNHost 设置图片 CDN 域名。
func (c *Client) SetCDNHost(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cdnHost = strings.TrimSpace(host)
}

// HTTPClient 获取底层 HTTP 客户端。
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// Timeout 返回请求超时。
func (c *Client) Timeout() time.Duration { return c.timeout }

// tokenAndTokenParam 包装底层签名计算，把返回顺序固定成「先进 token，再 tokenparam」，
// 避免 (tokenparam, token) 这种容易写反的老签名顺序。
func tokenAndTokenParam(timestamp, version, secret string) (token, tokenParam string) {
	tokenParam, token = utils.GetTokenAndTokenParam(timestamp, version, secret)
	return token, tokenParam
}

// Envelope 表示 API 服务端原始返回的密文响应结构体。
//
// Data 用 json.RawMessage：绝大多数接口返回的是 Base64 密文字符串，
// 但少数线路会直接返回明文 JSON 对象，两种都要能处理。
type Envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// payload 返回解密后的响应体。
func (e *Envelope) payload(timestamp, secret string) ([]byte, error) {
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return nil, fmt.Errorf("响应 data 字段为空 (code=%d message=%q)", e.Code, e.Message)
	}

	// 常规路径：data 是 Base64 密文字符串
	var encrypted string
	if err := json.Unmarshal(e.Data, &encrypted); err == nil {
		plain, err := utils.DecodeRespData(encrypted, timestamp, secret)
		if err != nil {
			return nil, fmt.Errorf("解密响应失败: %w", err)
		}
		return []byte(plain), nil
	}

	// 兜底：data 本身就是明文 JSON
	return e.Data, nil
}

// doGet 在 client 当前线路上发起一次 GET，校验状态码与业务码，返回解密后的响应体。
//
// 所有接口都走这里，好处是时间戳只生成一次、签名与解密必然一致。
func (c *Client) doGet(ctx context.Context, endpoint string) ([]byte, error) {
	return c.doGetAt(ctx, c.BaseURL(), endpoint)
}

// doGetAt 在**指定**线路上发起请求。
//
// 与 doGet 分开是为了并发探活：探活要同时试多条线路并各自计耗时，
// 若都去读 client 的 base_url，就得一边改一边读，既互相覆盖又踩数据竞争。
func (c *Client) doGetAt(ctx context.Context, baseURL, endpoint string) ([]byte, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("API 线路（base_url）未设置，请先探活或显式指定")
	}
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = baseURL + endpoint
	}

	// 时间戳在这里生成并注入 context，RoundTripper 会用它算签名头，
	// 下面的解密用同一个值——两边不可能错开。
	ts := fmt.Sprintf("%d", time.Now().Unix())
	ctx = WithTimestamp(ctx, ts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败 (%s): %w", endpoint, err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &NetworkError{URL: endpoint, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readLimited(resp.Body, 512)
		return nil, &StatusError{URL: endpoint, StatusCode: resp.StatusCode, Body: string(body)}
	}

	bodyBytes, err := readLimited(resp.Body, 32<<20)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败 (%s): %w", endpoint, err)
	}

	var envelope Envelope
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return nil, fmt.Errorf("响应不是合法的信封结构 (%s): %w (前 200 字节: %s)",
			endpoint, err, preview(bodyBytes))
	}

	// 服务端业务码：正常为 200；部分接口/线路用 0 表示成功。
	if envelope.Code != 200 && envelope.Code != 0 {
		return nil, &APIError{Code: envelope.Code, Message: envelope.Message}
	}

	plain, err := envelope.payload(ts, c.secret)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

// getJSON 发起请求并把解密结果反序列化到 v。
func (c *Client) getJSON(ctx context.Context, endpoint string, v any) error {
	plain, err := c.doGet(ctx, endpoint)
	if err != nil {
		return err
	}
	return decodeObject(plain, v, endpoint)
}

// getJSONAt 在指定线路上请求并反序列化（供探活用）。
func (c *Client) getJSONAt(ctx context.Context, baseURL, endpoint string, v any) error {
	plain, err := c.doGetAt(ctx, baseURL, endpoint)
	if err != nil {
		return err
	}
	return decodeObject(plain, v, endpoint)
}

// errEmptyArray 表示「想要一个对象，服务端却回了个空数组」。
//
// 实测（2026-09-21 抽了 20 本作品 + 三种接口）：
//
//	GET /album?id=<不存在的作品>      → `[]`
//	GET /comic_read?id=<不存在的章节> → `[]`
//	GET /forum?aid=<不存在的作品>     → **正常对象**（total=0、comments 为空数组）
//
// 也就是说空数组在这里是「这个东西不存在」的约定，属于**正常业务响应**。
// 以前它会掉进 json.Unmarshal 报一句 internal「解析响应 JSON 失败」，
// 宿主看到会以为工具坏了 —— 实际只是作品号填错了。
var errEmptyArray = errors.New("服务端返回空数组，该对象不存在")

// decodeObject 解出「期望是对象」的响应体。
//
// 与 decodeInto 只差一点：先把「空数组 = 不存在」这个服务端约定认掉，
// 转成 not_found。非空数组仍然按解析失败处理 —— 那说明接口形态真变了。
func decodeObject(plain []byte, v any, endpoint string) error {
	if isEmptyJSONArray(plain) {
		return protocol.Wrap(protocol.CodeNotFound, errEmptyArray, "%s", endpoint)
	}
	return decodeInto(plain, v)
}

// isEmptyJSONArray 报告响应体是不是一个空数组（允许前后空白）。
func isEmptyJSONArray(plain []byte) bool {
	return bytes.Equal(bytes.TrimSpace(plain), []byte("[]"))
}

// decodeInto 把解密后的响应体反序列化到 v。
func decodeInto(plain []byte, v any) error {
	if err := json.Unmarshal(plain, v); err != nil {
		return fmt.Errorf("解析响应 JSON 失败: %w (前 200 字节: %s)", err, preview(plain))
	}
	return nil
}
