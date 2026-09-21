package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// CommentModelAlbum 是作品评论区对应的 model 参数值。
//
// 服务端用同一个 /forum 接口承载多种留言板：作品、小说、影片……，靠 model 区分。
// APP 里作品评论固定传 1000。
const CommentModelAlbum = 1000

// DefaultCommentPageSize 是服务端一页评论的条数（APP 侧写死的分页大小）。
//
// 单独抽出来是因为「要下多少页」需要它：total 除以它向上取整。
const DefaultCommentPageSize = 20

// AlbumComments 获取作品评论第一页（GET /forum?model=1000&page=1&aid=<aid>）。
func (c *Client) AlbumComments(ctx context.Context, aid int) (*CommentListResult, error) {
	return c.AlbumCommentsPage(ctx, aid, 1)
}

// AlbumCommentsPage 获取作品评论指定页。
//
// page 从 1 开始；传 <=0 时按 1 处理。
func (c *Client) AlbumCommentsPage(ctx context.Context, aid, page int) (*CommentListResult, error) {
	if aid <= 0 {
		return nil, fmt.Errorf("评论接口 aid 非法: %d", aid)
	}
	if page <= 0 {
		page = 1
	}

	q := url.Values{}
	q.Set("model", strconv.Itoa(CommentModelAlbum))
	q.Set("page", strconv.Itoa(page))
	q.Set("aid", strconv.Itoa(aid))

	var result CommentListResult
	if err := c.getJSON(ctx, "/forum?"+q.Encode(), &result); err != nil {
		return nil, err
	}

	// 没有评论不是错误：详情页的 comment_total 为 0 时这里就是空 list，
	// 上层渲染一张「暂无评论」的图比抛异常更合理。
	return &result, nil
}

// TotalCount 把响应里以字符串给出的 total 转成整数，解析失败时返回 0。
func (r *CommentListResult) TotalCount() int {
	n, err := strconv.Atoi(r.Total)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// TotalPages 返回按 DefaultCommentPageSize 估算的总页数（至少 1 页）。
//
// 用于「把整本评论全抓下来再渲染」的场景——渲染成一张长图时需要全部评论，
// 只抓第一页会得到一张不完整的评论图。
func (r *CommentListResult) TotalPages() int {
	total := r.TotalCount()
	if total <= 0 {
		return 1
	}
	pages := (total + DefaultCommentPageSize - 1) / DefaultCommentPageSize
	if pages < 1 {
		pages = 1
	}
	return pages
}
