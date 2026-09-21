package utils

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DownloadTask 描述一次下载。
type DownloadTask struct {
	Url  string
	Dist string
}

// BatchDownloadResult 是单个任务的下载结果。
//
// Dist 一并返回：原实现靠 URL 反查任务下标，同一批里出现重复 URL 时会把结果
// 写到错误的位置；现在结果按下标一一对应，同时回传 Dist 便于调用方直接定位。
type BatchDownloadResult struct {
	Url  string
	Dist string
	// Attempts 实际发起请求的次数（含首次），用于区分「一次成功」和「重试后才成功」。
	Attempts int
	Err      error
}

// OK 报告是否下载成功。
func (r BatchDownloadResult) OK() bool { return r.Err == nil }

// statusError 表示「服务端明确回了非 200」的下载失败。
//
// 为什么单独定义类型、而不是让重试策略去 grep 错误文案：文案是会改的，
// 改一次就可能让 404 退化成"被当成临时故障反复重试" —— 这种退化**测试不会红**，
// 只是每次失败都白等 1s / 2s / 4s，很难被人发现。
// 顺带这样能覆盖全部 4xx，而不是只认识被列举出来的那几个。
type statusError struct {
	URL        string
	StatusCode int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("下载: %s 返回状态码 %d", e.URL, e.StatusCode)
}

// errNotAnImage 表示下载回来的内容不是图片 —— 通常是线路回了错误页。
//
// 同样是哨兵错误而非文案匹配：这类失败重试多少次都一样。
var errNotAnImage = errors.New("返回的内容不是图片（可能是线路返回了错误页），已丢弃")

// maxImageBytes 单张图片下载的体积上限。
//
// 图片本来就是小资源（实测单页几十 KB 到几 MB），但 CDN 或中间设备偶尔会回一坨
// 别的东西——错误页、被劫持的下载、chunked 的无限流。没有上限就是一路写满磁盘
// （而且是在 .part 里悄悄写满）。64 MiB 的余量已经远超任何真实漫画单页。
const maxImageBytes = 64 << 20

// defaultHTTPClientCache 缓存按代理配置创建的 client。
//
// 这是相对原实现的关键修正：原 Download() 每调用一次就 new 一个 http.Client，
// 下载一本 40 页的漫画就是 40 套连接池，等于完全没有连接复用（每张图都要重新
// TCP + TLS 握手）。这里按 proxy 维度复用同一个 client。
var (
	defaultHTTPClientCache sync.Map // proxy string -> *http.Client
	defaultClientMu        sync.Mutex
)

// NewHTTPClient 创建带代理和超时的 HTTP 客户端。
func NewHTTPClient(proxy string, timeout time.Duration) *http.Client {
	transport := newTransport(proxy)
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

func newTransport(proxy string) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxy != "" {
		if proxyURL, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	return transport
}

// sharedHTTPClient 返回按 proxy 复用的 client，避免每次下载都新建连接池。
func sharedHTTPClient(proxy string, timeout time.Duration) *http.Client {
	if c, ok := defaultHTTPClientCache.Load(proxy); ok {
		return c.(*http.Client)
	}
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	if c, ok := defaultHTTPClientCache.Load(proxy); ok {
		return c.(*http.Client)
	}
	c := NewHTTPClient(proxy, timeout)
	defaultHTTPClientCache.Store(proxy, c)
	return c
}

// Downloader 是带连接复用、重试与原子落盘的下载器。
//
// 推荐直接用这个类型；包级的 Download / BatchDownload 是保留的兼容入口。
type Downloader struct {
	client       *http.Client
	proxy        string
	timeout      time.Duration
	timeoutSet   bool
	userAgent    string
	headers      map[string]string
	maxRetries   int
	skipExisting bool
	validateImg  bool
}

// DownloaderOption 配置 Downloader。
type DownloaderOption func(*Downloader)

// WithProxy 设置 HTTP/HTTPS/SOCKS5 代理。
func WithProxy(proxy string) DownloaderOption {
	return func(d *Downloader) { d.proxy = proxy }
}

// WithTimeout 设置单次请求超时。
func WithTimeout(timeout time.Duration) DownloaderOption {
	return func(d *Downloader) {
		d.timeout = timeout
		d.timeoutSet = true
	}
}

// WithHTTPClient 注入自定义 client（注入后 proxy/timeout 不再生效）。
func WithHTTPClient(c *http.Client) DownloaderOption {
	return func(d *Downloader) { d.client = c }
}

// WithUserAgent 设置 User-Agent。
func WithUserAgent(ua string) DownloaderOption {
	return func(d *Downloader) { d.userAgent = ua }
}

// WithHeader 追加一个请求头（例如 Referer）。
func WithHeader(key, value string) DownloaderOption {
	return func(d *Downloader) {
		if d.headers == nil {
			d.headers = map[string]string{}
		}
		d.headers[key] = value
	}
}

// WithMaxRetries 设置单文件重试次数（总请求次数 = maxRetries + 1）。
func WithMaxRetries(n int) DownloaderOption {
	return func(d *Downloader) {
		if n < 0 {
			n = 0
		}
		d.maxRetries = n
	}
}

// WithSkipExisting 设置是否跳过已存在且非空的文件（断点续传）。
func WithSkipExisting(skip bool) DownloaderOption {
	return func(d *Downloader) { d.skipExisting = skip }
}

// WithValidateImage 设置是否校验响应确实是图片。
//
// 打开后（默认开启）会检查响应头几个字节是否为已知图片签名。线路异常时
// CDN 常常返回一个 HTML 错误页，如果不校验，它会作为 .webp 落盘，
// 然后在还原/合成阶段才以一个莫名的解码错误炸出来。
func WithValidateImage(v bool) DownloaderOption {
	return func(d *Downloader) { d.validateImg = v }
}

// NewDownloader 创建下载器。
//
// 未显式指定超时时复用按 proxy 维度共享的连接池（这样批量下载同一站点
// 才能共用 TCP/TLS 连接）；显式指定了不同超时则单独建池。
func NewDownloader(opts ...DownloaderOption) *Downloader {
	d := &Downloader{
		maxRetries:   3,
		skipExisting: false,
		validateImg:  true,
		timeout:      defaultDownloadTimeout,
		userAgent:    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.client == nil {
		if d.timeoutSet {
			d.client = NewHTTPClient(d.proxy, d.timeout)
		} else {
			d.client = sharedHTTPClient(d.proxy, d.timeout)
		}
	}
	if d.client.Timeout <= 0 {
		d.client.Timeout = d.timeout
	}
	return d
}

// defaultDownloadTimeout 是下载的默认单次请求超时。
const defaultDownloadTimeout = 60 * time.Second

// Download 下载单个文件；失败时不会留下半个文件。
func (d *Downloader) Download(ctx context.Context, rawURL, dst string) error {
	if rawURL == "" {
		return fmt.Errorf("下载: url 为空")
	}
	if dst == "" {
		return fmt.Errorf("下载: 目标路径为空")
	}

	if d.skipExisting {
		if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
			return nil
		}
	}
	if err := EnsureDir(filepath.Dir(dst)); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("下载: 构造请求失败 (%s): %w", rawURL, err)
	}
	if d.userAgent != "" {
		req.Header.Set("User-Agent", d.userAgent)
	}
	for k, v := range d.headers {
		req.Header.Set(k, v)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("下载: 请求失败 (%s): %w", rawURL, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return &statusError{URL: rawURL, StatusCode: resp.StatusCode}
	}

	// 先写到 .part，全部成功后再 rename，避免中途失败留下损坏文件被后续步骤当成有效输入。
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("下载: 创建文件 %q 失败: %w", tmp, err)
	}

	// 多读 1 字节来区分「正好等于上限」与「超过上限」。
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxImageBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载: 写入 %q 失败: %w", dst, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载: 关闭 %q 失败: %w", dst, closeErr)
	}
	if written == 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载: %s 返回了空响应", rawURL)
	}
	if written > maxImageBytes {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载: %s 响应超过单图上限 %d 字节（内容多半不是图片），已丢弃", rawURL, maxImageBytes)
	}

	if d.validateImg && !looksLikeImageFile(tmp) {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载: %s %w", rawURL, errNotAnImage)
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("下载: 落盘 %q 失败: %w", dst, err)
	}
	return nil
}

// DownloadWithRetry 带指数退避重试的下载。
//
// 4xx（除 408/429）属于确定性失败，重试没有意义，直接返回。
func (d *Downloader) DownloadWithRetry(ctx context.Context, rawURL, dst string) (int, error) {
	var lastErr error
	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			select {
			case <-ctx.Done():
				return attempt, WrapCanceled(ctx, lastErr)
			case <-time.After(delay):
			}
		}

		err := d.Download(ctx, rawURL, dst)
		if err == nil {
			return attempt + 1, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return attempt + 1, WrapCanceled(ctx, err)
		}
		if isDeterministicFailure(err) {
			// 404/403 之类重试多少次都一样，省掉等待
			return attempt + 1, err
		}
	}
	return d.maxRetries + 1, fmt.Errorf("下载失败（已尝试 %d 次）%s: %w", d.maxRetries+1, rawURL, lastErr)
}

// Batch 并发下载，结果与入参任务按下标一一对应。
//
// onDone 每完成一个任务回调一次（含失败的），done 为 1-based 已完成数量。
// onDone 在 worker 内串行调用，实现里不要做重活。
func (d *Downloader) Batch(
	ctx context.Context,
	tasks []DownloadTask,
	workers int,
	onDone func(done, total int, res BatchDownloadResult),
) []BatchDownloadResult {
	results := make([]BatchDownloadResult, len(tasks))
	if len(tasks) == 0 {
		return results
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}

	var (
		mu      sync.Mutex
		done    int
		nextIdx int
	)
	total := len(tasks)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if nextIdx >= total {
					mu.Unlock()
					return
				}
				i := nextIdx
				nextIdx++
				mu.Unlock()

				t := tasks[i]
				res := BatchDownloadResult{Url: t.Url, Dist: t.Dist}
				res.Attempts, res.Err = d.DownloadWithRetry(ctx, t.Url, t.Dist)
				results[i] = res

				mu.Lock()
				done++
				if onDone != nil {
					onDone(done, total, res)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return results
}

// ---- 包级兼容入口（原 API 保持不变，内部走共享连接池） ----

// Download 下载单个文件到 dist。
func Download(url, dist, proxy string) error {
	return NewDownloader(WithProxy(proxy)).Download(context.Background(), url, dist)
}

// DownloadWithRetry 带重试的下载。
func DownloadWithRetry(url, dist, proxy string, maxRetries int) error {
	_, err := NewDownloader(WithProxy(proxy), WithMaxRetries(maxRetries)).
		DownloadWithRetry(context.Background(), url, dist)
	return err
}

// BatchDownload 多线程下载。
//
// 注意：结果现在与 tasks 按下标一一对应（原实现按 URL 反查下标，
// 同一批里出现重复 URL 时结果会错位）。
func BatchDownload(tasks []DownloadTask, proxy string, workers int, maxRetries int) []BatchDownloadResult {
	d := NewDownloader(WithProxy(proxy), WithMaxRetries(maxRetries))
	return d.Batch(context.Background(), tasks, workers, nil)
}

// backoff 计算第 attempt 次重试前的等待时长（attempt 从 1 开始）。
//
// 1s → 2s → 4s，并加 ±30% 抖动：批量下载时所有 worker 同时失败会同时重试，
// 抖动可以把重试打散，避免把线路再打挂一次。
func backoff(attempt int) time.Duration {
	base := time.Second << uint(attempt-1)
	if base > 8*time.Second {
		base = 8 * time.Second
	}
	jitter := 1 + (rand.Float64()-0.5)*0.6
	return time.Duration(float64(base) * jitter)
}

// isDeterministicFailure 判断错误是否属于「重试也没用」。
//
// 判据是**错误类型**而不是错误文案（见 statusError / errNotAnImage）。
func isDeterministicFailure(err error) bool {
	if errors.Is(err, errNotAnImage) {
		return true
	}
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	// 408（请求超时）与 429（限流）是 4xx 里明确值得重试的两个。
	switch se.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	// 其余 4xx 一律确定性失败：重试只是白等退避。
	return se.StatusCode >= 400 && se.StatusCode < 500
}

// WrapCanceled 把 ctx 取消包装成带原因的错误，便于调用方区分「失败」和「被取消」。
func WrapCanceled(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		if cause != nil {
			return fmt.Errorf("已取消: %w（最后一次错误: %v）", err, cause)
		}
		return fmt.Errorf("已取消: %w", err)
	}
	return cause
}

// 常见图片格式的magic bytes。
var imageSignatures = [][]byte{
	{0xFF, 0xD8, 0xFF},     // jpeg
	{0x89, 'P', 'N', 'G'},  // png
	{'G', 'I', 'F', '8'},   // gif
	{'R', 'I', 'F', 'F'},   // webp (RIFF....WEBP)
	{'B', 'M'},             // bmp
	{'I', 'I', 0x2A, 0x00}, // tiff little-endian
	{'M', 'M', 0x00, 0x2A}, // tiff big-endian
	{0x00, 0x00, 0x00},     // 部分 avif/heif 的 ftyp 前导
}

// looksLikeImageFile 通过文件头判断是不是图片。
func looksLikeImageFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	head := make([]byte, 16)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	head = head[:n]
	if len(head) < 4 {
		return false
	}

	for _, sig := range imageSignatures {
		if bytes.HasPrefix(head, sig) {
			// avif/heif：需要看第 4..8 字节是不是 "ftyp"
			if len(sig) == 3 && bytes.Equal(head[4:8], []byte("ftyp")) {
				return true
			}
			if len(sig) != 3 {
				return true
			}
		}
	}
	// 兜底：交给标准库嗅探（能覆盖上面没列出的格式）
	ct := http.DetectContentType(head)
	return strings.HasPrefix(ct, "image/")
}
