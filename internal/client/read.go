package client

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
)

// ComicRead 获取阅读页数据（GET /comic_read?id=<aid>）。
//
// 这是下载链路的起点，返回体里有两样至关重要的东西：
//
//   - scramble_id：这张图要不要还原、还原成几段的阈值
//   - images[].image：带签名的图片直链，同时也是 CDN 域名的来源
//
// 注意 id 传的应该是**章节号**（就是详情页 series 里的那些 id）。传整本作品的
// 主 id 也能拿到内容，但如果这本作品有多个章节，拿到的是第一章。
func (c *Client) ComicRead(ctx context.Context, id int) (*ReadPageResult, error) {
	result, err := c.comicReadAt(ctx, c.BaseURL(), id)
	if err != nil {
		return nil, err
	}

	// 顺手把 CDN 域名从图片直链里学出来：这是拿到 CDN host 的唯一可靠来源，
	// 而且免费——本来就要请求这个接口。仅在调用方没有显式指定时才写入。
	if c.CDNHost() == "" {
		if host := HostFromURL(result.Images[0].Image); host != "" {
			c.SetCDNHost(host)
		}
	}

	return result, nil
}

// comicReadAt 在指定线路上读阅读页，且**不改动 client 的任何状态**。
//
// 探活专用：并发探多条线路时，不能一边并发一边改 client 的 base_url/cdn_host。
// 因此这里只负责取数据，CDN 域名由调用方自己决定要不要采纳。
func (c *Client) comicReadAt(ctx context.Context, baseURL string, id int) (*ReadPageResult, error) {
	if id <= 0 {
		return nil, fmt.Errorf("阅读页 id 非法: %d", id)
	}

	q := url.Values{}
	q.Set("id", strconv.Itoa(id))

	var result ReadPageResult
	if err := c.getJSONAt(ctx, baseURL, "/comic_read?"+q.Encode(), &result); err != nil {
		// 不存在的章节回的是空数组（见 errEmptyArray），换成人话再往上抛。
		if errors.Is(err, errEmptyArray) {
			return nil, protocol.Errorf(protocol.CodeNotFound, "章节 %d 不存在", id)
		}
		return nil, err
	}

	if result.Id == 0 {
		return nil, protocol.Errorf(protocol.CodeNotFound, "作品/章节 %d 没有可读内容（阅读页返回空体）", id)
	}
	if len(result.Images) == 0 {
		return nil, protocol.Errorf(protocol.CodeNotFound, "作品 %d（%s）返回了 0 张图片，可能已下架或权限不足", id, result.Name)
	}
	return &result, nil
}

// HostFromURL 从 URL 里取出 host（不含端口与协议），取不到时返回空串。
//
// 用于从图片直链反向识别 CDN 域名：
//
//	https://cdn-msp3.jmapiproxy1.cc/media/photos/1423323/00001.webp?t=178
//	→ cdn-msp3.jmapiproxy1.cc
func HostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
