package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// Search 搜索作品（GET /search?search_query=&o=&page=）。
//
// order 取值见 SearchOrders；空串表示默认排序。
func (c *Client) Search(ctx context.Context, keyword string, page int, order string) (*SearchResult, error) {
	return c.SearchWithParams(ctx, SearchParams{
		Keyword: keyword,
		Page:    page,
		Order:   order,
	})
}

// SearchWithParams 是 Search 的参数对象版本。
func (c *Client) SearchWithParams(ctx context.Context, params SearchParams) (*SearchResult, error) {
	if params.Keyword == "" {
		return nil, fmt.Errorf("搜索关键词不能为空")
	}
	if params.Page <= 0 {
		params.Page = 1
	}

	q := url.Values{}
	q.Set("search_query", params.Keyword)
	// o 即使为空也必须出现：服务端对这个参数的存在性有要求。
	q.Set("o", params.Order)
	if params.Page > 1 {
		q.Set("page", strconv.Itoa(params.Page))
	}

	// 这里不走 getJSON：搜索结果的 RawJSON 要留给上层做兜底
	// （历史数据的 update_at 有时是字符串有时是数字，反序列化失败时还能拿原文）。
	plain, err := c.doGet(ctx, "/search?"+q.Encode())
	if err != nil {
		return nil, err
	}

	var result SearchResult
	if err := json.Unmarshal(plain, &result); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败: %w (前 200 字节: %s)", err, preview(plain))
	}
	result.RawJSON = plain
	return &result, nil
}

// 排序方式常量（对齐 APP 的搜索参数）。
const (
	OrderLatest = "mr" // 最新
	OrderView   = "mv" // 最多浏览
	OrderPic    = "mp" // 最多图片
	OrderLike   = "tf" // 最多点赞
	// 榜单
	OrderMonthRanking = "mv_m"
	OrderWeekRanking  = "mv_w"
	OrderDayRanking   = "mv_t"
)

// 分类常量（对齐 APP 的 category 参数）。
const (
	CategoryAll     = "0"
	CategoryDoujin  = "doujin"
	CategorySingle  = "single"
	CategoryShort   = "short"
	CategoryOther   = "another"
	CategoryHanman  = "hanman"
	CategoryMeiman  = "meiman"
	CategoryCosplay = "doujin_cosplay"
	Category3D      = "3d"
)
