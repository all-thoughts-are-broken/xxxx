package client

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
)

// AlbumDetail 获取作品详情（GET /album?id=<aid>）。
//
// 对应 APP 的「作品详情」接口，返回体是解密后的 JSON。
func (c *Client) AlbumDetail(ctx context.Context, id int) (*AlbumDetailResult, error) {
	if id <= 0 {
		return nil, fmt.Errorf("作品号非法: %d", id)
	}

	q := url.Values{}
	q.Set("id", strconv.Itoa(id))

	var result AlbumDetailResult
	if err := c.getJSON(ctx, "/album?"+q.Encode(), &result); err != nil {
		// 服务端对不存在的作品回的是空数组（见 errEmptyArray）。
		// 在这里换成一句人话，宿主就不用去猜 「[] 解析失败」是什么意思了。
		if errors.Is(err, errEmptyArray) {
			return nil, protocol.Errorf(protocol.CodeNotFound, "作品 %d 不存在", id)
		}
		return nil, err
	}

	if result.Id == 0 {
		// 兜底：万一服务端改用空对象（`{}`）表示不存在，这里也要拦住，
		// 否则上层会拿着一堆零值继续往下走，最后在某个奇怪的地方失败。
		return nil, protocol.Errorf(protocol.CodeNotFound, "作品 %d 不存在，或接口返回了空详情", id)
	}
	if result.Id != id {
		// 与搜索的 redirect_aid 同源：传章节号时会被重定向到所属作品
		// （series_id 指向第一章）。这不算错误，仅记录在 Description 之外的地方没必要。
		// 上层可用 result.Id 拿到真实作品号。
		result.RequestedId = id
	}

	return &result, nil
}
