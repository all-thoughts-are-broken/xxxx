package client

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ============ 请求参数 ============
//
// 只列真正会被用到的几个。接口本身的调用都是 Client 上的方法
// （AlbumDetail / ComicRead / AlbumComments / Search），这些结构体
// 是给「需要一次性把参数打包传下去」的场景（例如服务层转发）用的。

// SearchParams 搜索请求参数。
type SearchParams struct {
	Keyword string // 搜索关键词
	Page    int    // 分页页码 (默认从 1 开始)
	Order   string // 排序方式 (例如: "mr" - 最新, "mv" - 最多浏览, "tf" - 最多点赞)
}

// AlbumDetailParams 作品详情请求参数。
type AlbumDetailParams struct {
	Id int // aid
}

// CommentListParams 评论列表请求参数。
//
// 原字段名是 Moded，那是 Mode 的笔误；接口上真实的参数名是 model。
type CommentListParams struct {
	Model int // 固定 1000（APP 的作品评论频道）
	Page  int
	Aid   int
}

// CommentLissParams 保留旧名，等价于 CommentListParams。
//
// Deprecated: 用 CommentListParams。
type CommentLissParams = CommentListParams

// ReadPageParams 阅读页请求参数。
type ReadPageParams struct {
	Id int
}

// ============ 响应结构 ============
//
// Envelope 定义在 client.go（它带解密方法），这里不再重复声明。

type Category struct {
	Id    *string `json:"id"`
	Title *string `json:"title"`
}

type Categorysub = Category

type Series struct {
	Id   string `json:"id"`
	Name string `json:"name"`
	Sort string `json:"sort"`
}

type Relatedlist struct {
	Id     string `json:"id"`
	Author string `json:"author"`
	Name   string `json:"name"`
	Image  string `json:"image"`
}

type Badge struct {
	Content string `json:"content"`
	Name    string `json:"name"`
	Id      string `json:"id"`
}

type Expinfo struct {
	LevelName string `json:"level_name"`
	Level     int    `json:"level"`
	// NextLevelExp / ExpPercent 都可能是小数：实测 expPercent 会返回
	// 86.21869488536156 这种值。声明成 int 会让整个评论列表解析失败
	// （UnmarshalTypeError 直接冒到调用方），于是渲染评论区整条链路挂掉。
	NextLevelExp int     `json:"nextLevelExp"`
	Exp          string  `json:"exp"`
	ExpPercent   float64 `json:"expPercent"`
	Uid          string  `json:"uid"`
	Badges       []Badge `json:"badges"`
}

type Reply struct {
	CID       string  `json:"CID"`
	UID       string  `json:"UID"`
	Username  string  `json:"username"`
	Nickname  string  `json:"nickname"`
	Likes     string  `json:"likes"`
	Gender    string  `json:"gender"`
	UpdateAt  string  `json:"update_at"`
	Addtime   string  `json:"addtime"`
	ParentCID string  `json:"parent_CID"`
	Photo     string  `json:"photo"`
	Content   string  `json:"content"`
	Expinfo   Expinfo `json:"expinfo"`
	Spoiler   string  `json:"spoiler"`
}

type List struct {
	AID       string   `json:"AID"`
	BID       *string  `json:"BID"`
	CID       string   `json:"CID"`
	NID       *string  `json:"NID"`
	NCID      *string  `json:"NCID"`
	UID       string   `json:"UID"`
	Username  string   `json:"username"`
	Nickname  string   `json:"nickname"`
	Likes     string   `json:"likes"`
	Gender    string   `json:"gender"`
	UpdateAt  string   `json:"update_at"`
	Addtime   string   `json:"addtime"`
	ParentCID string   `json:"parent_CID"`
	Expinfo   Expinfo  `json:"expinfo"`
	Name      string   `json:"name"`
	Content   string   `json:"content"`
	Photo     string   `json:"photo"`
	Spoiler   string   `json:"spoiler"`
	Replys    *[]Reply `json:"replys"`
}

type Images struct {
	Page  int    `json:"page"`
	Image string `json:"image"`
}

// ComicItem 搜索结果中的漫画条目简要信息。
type ComicItem struct {
	ID          string      `json:"id"`
	Author      string      `json:"author"`
	Description *string     `json:"description"`
	Name        string      `json:"name"`
	Image       string      `json:"image"`
	Category    Category    `json:"category"`
	CategorySub Categorysub `json:"category_sub"`
	Liked       bool        `json:"liked"`
	IsFavorite  bool        `json:"is_favorite"`
	UpdateAt    int64       `json:"update_at"`
}

// SearchResult 解密后的搜索结果结构体。
type SearchResult struct {
	SearchQuery string          `json:"search_query"`
	Total       int             `json:"total"`
	RedirectAid *string         `json:"redirect_aid"` // 通过搜索一个本子号比如10086，如果这个有值那么就说明这个本子是存在的，之后就可以拿信息拿评论拿资源
	Content     []ComicItem     `json:"content"`
	RawJSON     json.RawMessage `json:"-"` // 保存解密后的原始 JSON 数据
}

type AlbumDetailResult struct {
	Id           int           `json:"id"`
	Name         string        `json:"name"` // 作品名
	Images       interface{}   `json:"images"`
	Addtime      string        `json:"addtime"`     // 发布时间
	Description  string        `json:"description"` // 简介
	TotalViews   string        `json:"total_views"` // 总浏览数
	Likes        string        `json:"likes"`       // 喜欢数
	Series       []Series      `json:"series"`      // 章节
	SeriesId     string        `json:"series_id"`
	RealLink     string        `json:"real_link"`
	CommentTotal string        `json:"comment_total"` // 评论数
	Author       []string      `json:"author"`        // 作者
	Tags         []string      `json:"tags"`          // 标签
	Works        interface{}   `json:"works"`
	Actors       interface{}   `json:"actors"`
	RelatedList  []Relatedlist `json:"related_list"` // 更多相关
	Liked        bool          `json:"liked"`
	IsFavorite   bool          `json:"is_favorite"`
	IsAids       bool          `json:"is_aidss"`
	Price        string        `json:"price"`
	Purchased    string        `json:"purchased"`

	// RequestedId 记录调用方最初请求的作品号。
	//
	// 传章节号时服务端会重定向到所属作品（Id 变成真实作品号），
	// 两者不等就说明发生了重定向，上层可据此提示用户。
	RequestedId int `json:"-"`
}

type CommentListResult struct {
	List  []List `json:"list"`
	Total string `json:"total"`
}

type ReadPageResult struct {
	Id         int      `json:"id"`
	ScrambleId string   `json:"scramble_id"`
	Name       string   `json:"name"`
	TotalPage  int      `json:"total_page"`
	Images     []Images `json:"images"`
	Addtime    string   `json:"addtime"`
	Adddt      string   `json:"adddt"`
	SeriesId   string   `json:"series_id"` // 如果这个是别的作品中的某章那么这个就指向该作品的第一章（第一章能拿到全貌，比如有多少章啥的）
	RealLink   string   `json:"real_link"`
	IsFavorite bool     `json:"is_favorite"`
	Liked      bool     `json:"liked"`
}

// ScrambleID 把 JSON 里以字符串给出的 scramble_id 转成整数。
//
// 服务端返回的是字符串（"220980"）；解析失败时返回 0，
// 由 utils.CountImageSegments 自行回退到默认阈值。
func (r *ReadPageResult) ScrambleID() int {
	n, err := strconv.Atoi(strings.TrimSpace(r.ScrambleId))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// ImageURLs 返回所有页的图片直链，按 page 升序。
//
// 服务端已经按页序返回，这里只是把 nil 项与空串滤掉，避免把
// 一个空 URL 交给下载器（那会变成对 base_url 的一次无意义请求）。
func (r *ReadPageResult) ImageURLs() []string {
	urls := make([]string, 0, len(r.Images))
	for _, img := range r.Images {
		if u := strings.TrimSpace(img.Image); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}
