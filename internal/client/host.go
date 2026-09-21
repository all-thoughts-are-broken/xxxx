package client

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本文件集中放「拼 URL」与「探线路」两类逻辑。
//
// 图片相关的 URL 有三套不同的规则，很容易记混，所以统一收在这里：
//
//	封面   https://<cdn>/media/albums/<aid>_3x4.jpg
//	头像   https://<cdn>/media/users/<photo>
//	勋章   https://<cdn>/static/resources/images/...<photo>   （photo 已是完整路径）
//	阅读图 https://<cdn>/media/photos/<aid>/<page:5 位>.webp
//
// 注意 cdn host 与 API base_url 是**两个不同的域名**：base_url 用于取接口
// （返回加密 JSON），cdn host 用于取图片（返回裸图）。它们通常不在同一个域下。

// BuildAlbumCoverURL 构建作品封面图 URL。
//
// 例：aid=1423323 → https://cdn-msp3.jmapiproxy1.cc/media/albums/1423323_3x4.jpg
//
// host 为空时返回空串（调用方据此降级为「无封面」而不是拼出一个坏 URL）。
func BuildAlbumCoverURL(aid int, host string) string {
	if aid <= 0 || strings.TrimSpace(host) == "" {
		return ""
	}
	return fmt.Sprintf("%s/media/albums/%d_3x4.jpg", cdnBase(host), aid)
}

// BuildUserAvatarURL 构建用户头像 URL。
//
// 例：photo="nopic-Male.gif" → https://cdn-msp.jmapiproxy3.cc/media/users/nopic-Male.gif
func BuildUserAvatarURL(host, photo string) string {
	photo = strings.TrimSpace(photo)
	if photo == "" {
		return ""
	}
	// 接口偶尔会直接给完整 URL，这种情况原样返回。
	if strings.HasPrefix(photo, "http://") || strings.HasPrefix(photo, "https://") {
		return photo
	}
	if strings.TrimSpace(host) == "" {
		return ""
	}
	return cdnBase(host) + "/media/users/" + strings.TrimLeft(photo, "/")
}

// BuildBadgeImageURL 构建勋章图片 URL。
//
// photo 是相对站点的完整路径（例如
// `/static/resources/images/勋章/2021.8勋章/maidragon_8.png`），
// 所以这里只做 host 拼接，不再补任何路径前缀。
func BuildBadgeImageURL(host, photo string) string {
	photo = strings.TrimSpace(photo)
	if photo == "" {
		return ""
	}
	if strings.HasPrefix(photo, "http://") || strings.HasPrefix(photo, "https://") {
		return photo
	}
	if strings.TrimSpace(host) == "" {
		return ""
	}
	return cdnBase(host) + "/" + strings.TrimLeft(photo, "/")
}

// BuildReadImageURL 构建阅读页第 page 页的图片 URL。
//
// 例：aid=1423323, page=11 → https://<cdn>/media/photos/1423323/00011.webp
//
// 仅在接口没直接给 images[].image 时需要（例如自己按页号重算重试）。
// 这类 URL 不带 ?t= 签名，因此**可能被 CDN 拒绝**——优先用接口返回的直链。
func BuildReadImageURL(aid, page int, host string) string {
	if aid <= 0 || page <= 0 || strings.TrimSpace(host) == "" {
		return ""
	}
	return fmt.Sprintf("%s/media/photos/%d/%s.webp", cdnBase(host), aid, PadPage(page))
}

// PadPage 把页码格式化成 5 位（服务端的文件名规则：1 → "00001"）。
func PadPage(page int) string {
	s := strconv.Itoa(page)
	if len(s) >= 5 {
		return s
	}
	return strings.Repeat("0", 5-len(s)) + s
}

// cdnBase 把 host 归一化成 "https://host" 形式。
//
// 兼容调用方传入裸 host（cdn-msp3.xxx.cc）、带协议（https://cdn...）、
// 或带尾斜杠三种写法，避免拼出 "https://https://..." 这种坏 URL。
func cdnBase(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimRight(host, "/")
	}
	return "https://" + strings.TrimRight(host, "/")
}

// ProbeResult 描述一次线路探活的结果。
type ProbeResult struct {
	// BaseURL 探活成功的线路；全都失败时为空串。
	BaseURL string
	// CDNHost 从成功线路的阅读页里带出来的 CDN 域名。
	CDNHost string
	// Attempts 记录每条线路的失败原因，便于把「为什么全挂了」报给宿主。
	Attempts []ProbeAttempt
}

// ProbeAttempt 是一条线路的探活记录。
type ProbeAttempt struct {
	BaseURL string
	// LatencyMS 一次真实阅读页请求的耗时（毫秒）。失败时也有参考意义 ——
	// 连接被拒通常很快，而超时会顶到超时上限，两者能区分开。
	LatencyMS int64
	// CDNHost 该线路顺带探到的图片 CDN 域名（成功时才有值）。
	CDNHost string
	Err     error
}

// OK 报告是否探到可用线路。
func (r ProbeResult) OK() bool { return r.BaseURL != "" }

// Error 把全部失败原因汇总成一句话。
func (r ProbeResult) Error() string {
	if r.OK() {
		return ""
	}
	var b strings.Builder
	b.WriteString("所有候选线路都不可用")
	for _, a := range r.Attempts {
		if a.Err != nil {
			fmt.Fprintf(&b, "; %s: %v", a.BaseURL, a.Err)
		}
	}
	return b.String()
}

// probeComicRead 在指定线路上做一次真实阅读页请求，返回该线路带出的
// CDN 域名与耗时。**不改动 client 状态**，因此可以并发调用。
//
// 探活为什么用最重的「阅读页」接口：它一次性验证四件事 ——
// 线路可达、密钥/签名正确（要能解密才算通过）、接口版本没变，并免费带回 CDN 域名。
//
// 只看 HTTP 状态码是不行的：作废的线路照样可能返回 200 和一个结构完全合法的
// 信封，只是响应解不开而已（见 config.DefaultBaseURLs 里 cdnbea 的注释）。
func (c *Client) probeComicRead(ctx context.Context, baseURL string, aid int) (string, int64, error) {
	start := time.Now()
	res, err := c.comicReadAt(ctx, baseURL, aid)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return "", ms, err
	}
	return HostFromURL(res.Images[0].Image), ms, nil
}

// cleanCandidates 去空白、去尾斜杠、去重，保持原顺序。
func cleanCandidates(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimRight(strings.TrimSpace(candidate), "/")
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

// ProbeBaseURLs 按顺序试每条候选线路，返回第一个可用的。
//
// 成功时会**把 client 的 base_url / cdn_host 就地切过去**，调用方直接用即可。
// 全部失败时返回的结果里带每条线路的错误原因，client 状态保持探活前的值。
//
// 如果面对的是"已知的一批线路"（比如动态清单给的），优先用 ProbeFastest：
// 线路之间的延迟可能差一倍以上，而下载一本作品要打几百次接口。
// 这里保留顺序探测，是为了「兜底列表」这种掺着失效线路的场景 —— 先试
// 已知优先级更高的，命中即停。
func (c *Client) ProbeBaseURLs(ctx context.Context, candidates []string, probeAID int) ProbeResult {
	if probeAID <= 0 {
		probeAID = 10086
	}
	list := cleanCandidates(candidates)
	originalBase, originalCDN := c.BaseURL(), c.CDNHost()
	res := ProbeResult{Attempts: make([]ProbeAttempt, 0, len(list))}

	for _, candidate := range list {
		if err := ctx.Err(); err != nil {
			res.Attempts = append(res.Attempts, ProbeAttempt{BaseURL: candidate, Err: err})
			c.SetBaseURL(originalBase)
			c.SetCDNHost(originalCDN)
			return res
		}

		cdn, ms, err := c.probeComicRead(ctx, candidate, probeAID)
		res.Attempts = append(res.Attempts, ProbeAttempt{
			BaseURL: candidate, LatencyMS: ms, CDNHost: cdn, Err: err,
		})
		if err != nil {
			continue
		}

		c.SetBaseURL(candidate)
		c.SetCDNHost(cdn)
		res.BaseURL, res.CDNHost = candidate, cdn
		return res
	}

	c.SetBaseURL(originalBase)
	c.SetCDNHost(originalCDN)
	return res
}

// ProbeFastest 并发探测所有候选线路，采用**延迟最低**的那条。
//
// 为什么值得并发全试一遍：实测同一时刻不同线路差到 1.1s vs 1.8s，
// 下载一本作品要打几百次接口，这个倍数会被放大成实打实的时间。
// 而且并发探测的总耗时接近"最快那条"，反而比顺序试还快。
//
// workers <= 0 时用 4。成功时把 client 的 base_url / cdn_host 切到赢家；
// 全部失败则保持原状，并带回每条的耗时/原因。
func (c *Client) ProbeFastest(ctx context.Context, candidates []string, probeAID int, workers int) ProbeResult {
	if probeAID <= 0 {
		probeAID = 10086
	}
	if workers <= 0 {
		workers = 4
	}

	list := cleanCandidates(candidates)
	originalBase, originalCDN := c.BaseURL(), c.CDNHost()

	// 单条线路没必要起 goroutine，直接走同步路径。
	if len(list) == 1 {
		return c.ProbeBaseURLs(ctx, list, probeAID)
	}

	attempts := make([]ProbeAttempt, len(list))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, candidate := range list {
		wg.Add(1)
		go func(i int, candidate string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 每个 goroutine 只写自己的下标，互不干扰。
			cdn, ms, err := c.probeComicRead(ctx, candidate, probeAID)
			attempts[i] = ProbeAttempt{BaseURL: candidate, LatencyMS: ms, CDNHost: cdn, Err: err}
		}(i, candidate)
	}
	wg.Wait()

	res := ProbeResult{Attempts: attempts}

	// 在所有成功的线路里取耗时最小的。
	best := -1
	for i := range attempts {
		if attempts[i].Err != nil {
			continue
		}
		if best < 0 || attempts[i].LatencyMS < attempts[best].LatencyMS {
			best = i
		}
	}
	if best < 0 {
		c.SetBaseURL(originalBase)
		c.SetCDNHost(originalCDN)
		return res
	}

	res.BaseURL = attempts[best].BaseURL
	res.CDNHost = attempts[best].CDNHost
	c.SetBaseURL(res.BaseURL)
	c.SetCDNHost(res.CDNHost)
	return res
}
