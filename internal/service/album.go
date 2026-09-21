package service

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// ============ 作品详情 ============

// ChapterBrief 是章节的简要信息。
type ChapterBrief struct {
	// ID 章节号（用它调 comic_read 能拿到该章的内容）。
	ID int `json:"id"`
	// Name 章节名。服务端为空时上层会补「第 N 章」。
	Name string `json:"name"`
	// Sort 章节序号（1-based）。
	Sort int `json:"sort"`
}

// AlbumDetail 是提供给宿主的作品详情视图。
//
// 它是 client.AlbumDetailResult 的「已归一化」版本：接口返回的
// likes / total_views / comment_total 都是字符串，这里统一成整数；
// description 里的 Markdown 标记也已剥离；章节列表按 sort 排好序。
type AlbumDetail struct {
	ID          int            `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Author      []string       `json:"author"`
	Tags        []string       `json:"tags"`
	Likes       int            `json:"likes"`
	Views       int            `json:"views"`
	Comments    int            `json:"comments"`
	AddTime     string         `json:"add_time"`
	Chapters    []ChapterBrief `json:"chapters"`
	// CoverURL 封面直链（<cdn>/media/albums/<aid>_3x4.jpg）。
	CoverURL string `json:"cover_url"`
	// RedirectedFrom 非零表示：请求的是章节号，服务端重定向到了所属作品。
	RedirectedFrom int  `json:"redirected_from,omitempty"`
	IsFavorite     bool `json:"is_favorite"`
}

// AlbumDetailRequest 是作品详情请求。
type AlbumDetailRequest struct {
	// ID 作品号，也可以是章节号（此时结果里会带 RedirectedFrom）。
	ID int `json:"id"`
}

// AlbumDetail 拉取作品详情。
func (s *Service) AlbumDetail(ctx context.Context, prog ProgressFunc, req AlbumDetailRequest) (*AlbumDetail, error) {
	if req.ID <= 0 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "作品号非法: %d", req.ID)
	}

	cli, cfg, err := s.ensureClient(ctx, prog)
	if err != nil {
		return nil, err
	}

	raw, err := cli.AlbumDetail(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if cfg.CDNHost == "" && cli.CDNHost() != "" {
		cfg.CDNHost = cli.CDNHost()
	}

	return toAlbumDetail(raw, cfg.CDNHost), nil
}

// toAlbumDetail 把接口响应归一化成视图结构。
func toAlbumDetail(raw *client.AlbumDetailResult, cdnHost string) *AlbumDetail {
	out := &AlbumDetail{
		ID:          raw.Id,
		Name:        trimUnicodeSpace(raw.Name),
		Description: utils.StripMarkdown(raw.Description),
		Author:      raw.Author,
		Tags:        raw.Tags,
		Likes:       atoiSafe(raw.Likes),
		Views:       atoiSafe(raw.TotalViews),
		Comments:    atoiSafe(raw.CommentTotal),
		AddTime:     formatAddTime(raw.Addtime, ""),
		CoverURL:    client.BuildAlbumCoverURL(raw.Id, cdnHost),
		IsFavorite:  raw.IsFavorite,
	}

	if raw.RequestedId != 0 && raw.RequestedId != raw.Id {
		out.RedirectedFrom = raw.RequestedId
	}

	for _, ch := range raw.Series {
		id, _ := strconv.Atoi(strings.TrimSpace(ch.Id))
		sort, _ := strconv.Atoi(strings.TrimSpace(ch.Sort))
		name := trimUnicodeSpace(ch.Name)
		if name == "" && sort > 0 {
			name = "第" + itoa(sort) + "章"
		}
		out.Chapters = append(out.Chapters, ChapterBrief{ID: id, Name: name, Sort: sort})
	}

	sortChapters(out.Chapters)
	return out
}

// sortChapters 按 sort 升序排列章节。
//
// 接口没给序号（Sort <= 0）的章节整体挪到末尾，并保持它们之间的原有顺序。
//
// 这里用 sort.SliceStable 而不是手写插入排序：后者在「部分元素不参与排序」时
// 极易写错 —— 被 continue 跳过的元素会变成屏障，挡住后面元素前移，
// 结果是排完序仍然不升序（实测踩到过）。
func sortChapters(chs []ChapterBrief) {
	if len(chs) < 2 {
		return
	}
	sort.SliceStable(chs, func(i, j int) bool {
		a, b := chs[i].Sort, chs[j].Sort
		// 无序号的一律排在有序号之后
		if a <= 0 {
			return false
		}
		if b <= 0 {
			return true
		}
		return a < b
	})
}

// formatAddTime 把 unix 秒时间戳字符串格式化成可读时间。
//
// 解析失败时返回 fallback（调用方常把接口给的 adddt 传进来）。
func formatAddTime(unix string, fallback string) string {
	if ts, err := strconv.ParseInt(strings.TrimSpace(unix), 10, 64); err == nil && ts > 0 {
		return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
	}
	return fallback
}

// atoiSafe 解析整数，失败返回 0（接口把这些计数字段给成了字符串）。
func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ============ 阅读页 ============

// ReadPageRequest 是阅读页请求。
type ReadPageRequest struct {
	// ID 作品号或章节号。
	ID int `json:"id"`
}

// ReadPage 拉取阅读页（图片直链 + scramble_id）。
//
// 结果里同时给出「已还原好的页码 → 文件名」映射所需的全部信息，
// 宿主可以直接拿 Images 里的直链去下载。
func (s *Service) ReadPage(ctx context.Context, prog ProgressFunc, req ReadPageRequest) (*client.ReadPageResult, error) {
	if req.ID <= 0 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "阅读页 id 非法: %d", req.ID)
	}

	cli, cfg, err := s.ensureClient(ctx, prog)
	if err != nil {
		return nil, err
	}

	page, err := cli.ComicRead(ctx, req.ID)
	if err != nil {
		return nil, err
	}

	// 学到的 CDN 域名写回配置（仅内存，本次进程内有效；不落盘的理由见
	// ensureClient 的注释），后续封面/头像下载就能直接用。
	if cfg.CDNHost == "" && cli.CDNHost() != "" {
		if _, err := s.cfg.Set(mustJSON(map[string]any{"cdn_host": cli.CDNHost()})); err != nil {
			prog.report(Progress{Stage: "read", Message: "cdn_host 写回配置失败（不影响本次读取）: " + err.Error()})
		}
	}
	return page, nil
}

// ============ 评论 ============

// AlbumCommentsRequest 是评论请求。
type AlbumCommentsRequest struct {
	AID int `json:"aid"`
	// AllPages 为 true 时抓取全部页（渲染评论长图需要完整数据）。
	AllPages bool `json:"all_pages"`
	// MaxPages 限制最多抓多少页（AllPages=true 时生效）；<=0 表示不限制。
	MaxPages int `json:"max_pages"`
}

// CommentList 是评论列表视图。
type CommentList struct {
	AID      int           `json:"aid"`
	Total    int           `json:"total"`
	Comments []client.List `json:"comments"`
	Pages    int           `json:"pages"`
	// FailedPages 记录抓取失败的页码（AllPages 模式下部分页失败不算整体失败）。
	FailedPages []int `json:"failed_pages,omitempty"`
}

// AlbumComments 拉取作品评论。
func (s *Service) AlbumComments(ctx context.Context, prog ProgressFunc, req AlbumCommentsRequest) (*CommentList, error) {
	if req.AID <= 0 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "评论 aid 非法: %d", req.AID)
	}

	cli, _, err := s.ensureClient(ctx, prog)
	if err != nil {
		return nil, err
	}

	first, err := cli.AlbumCommentsPage(ctx, req.AID, 1)
	if err != nil {
		return nil, err
	}

	out := &CommentList{AID: req.AID, Total: first.TotalCount(), Comments: first.List, Pages: 1}

	if !req.AllPages || len(first.List) == 0 {
		return out, nil
	}

	pages := first.TotalPages()
	if req.MaxPages > 0 && pages > req.MaxPages {
		pages = req.MaxPages
	}

	prog.report(Progress{
		Stage:   "comment",
		Done:    1,
		Total:   pages,
		Message: "已抓取评论第 1 页",
	})

	for p := 2; p <= pages; p++ {
		if err := ctx.Err(); err != nil {
			return out, protocol.Wrap(protocol.CodeCanceled, err, "抓取评论被中断，已抓取 %d/%d 页", p-1, pages)
		}

		list, err := cli.AlbumCommentsPage(ctx, req.AID, p)
		if err != nil {
			// 单页失败不中断整体：评论图缺一页总比整张图渲染不出来强。
			out.FailedPages = append(out.FailedPages, p)
			prog.report(Progress{
				Stage:   "comment",
				Done:    p,
				Total:   pages,
				Message: "第 " + itoa(p) + " 页抓取失败，已跳过",
			})
			continue
		}
		out.Comments = append(out.Comments, list.List...)
		out.Pages = p
		prog.report(Progress{
			Stage:   "comment",
			Done:    p,
			Total:   pages,
			Message: "已抓取评论第 " + itoa(p) + " 页",
		})
	}

	return out, nil
}

// ============ 渲染 ============

// RenderRequest 是渲染类命令的通用请求。
type RenderRequest struct {
	// AID 作品号。
	AID int `json:"aid"`
	// OutputPath 输出路径；为空时自动落在 output_dir 下。
	OutputPath string `json:"output_path"`
	// Scale 超采样倍率；<=0 用渲染层默认值。
	Scale float64 `json:"scale"`
}

// RenderResult 是渲染结果。
type RenderResult struct {
	OutputPath string `json:"output_path"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	ElapsedMS  int64  `json:"elapsed_ms"`
}

// RenderAlbumDetailImage 渲染作品详情卡片。
func (s *Service) RenderAlbumDetailImage(ctx context.Context, prog ProgressFunc, req RenderRequest) (*RenderResult, error) {
	start := time.Now()
	cfg := s.Config()

	detail, err := s.AlbumDetail(ctx, prog, AlbumDetailRequest{ID: req.AID})
	if err != nil {
		return nil, err
	}

	// 封面：接口给的 3x4.jpg 缩略图；下不下来就退化成占位封面（不报错）。
	coverPath := ""
	if detail.CoverURL != "" {
		prog.report(Progress{Stage: "render", Message: "正在下载封面"})
		dst := filepath.Join(cacheDir(cfg), "cover_"+itoa(detail.ID)+".jpg")
		if err := s.downloaderFor(cfg).Download(ctx, detail.CoverURL, dst); err == nil {
			coverPath = dst
		} else {
			prog.report(Progress{Stage: "render", Message: "封面下载失败，使用占位封面: " + err.Error()})
		}
	}

	card := utils.AlbumCard{
		ID:        itoa(detail.ID),
		Name:      detail.Name,
		Desc:      detail.Description,
		Like:      detail.Likes,
		Comment:   detail.Comments,
		View:      detail.Views,
		Tags:      detail.Tags,
		Author:    detail.Author,
		AddTime:   detail.AddTime,
		Chapters:  chapterNames(detail.Chapters),
		Reading:   readingIndex(detail.Chapters, req.AID),
		CoverPath: coverPath,
	}
	if len(detail.Chapters) > 0 {
		card.Series = detail.Chapters[len(detail.Chapters)-1].Sort
	}

	out := req.OutputPath
	if out == "" {
		out = filepath.Join(cfg.OutputDir, sanitizeOr(itoa(detail.ID), "album")+"_card.png")
	}

	prog.report(Progress{Stage: "render", Message: "正在渲染详情卡片"})
	if err := utils.RenderAlbumCard(card, out); err != nil {
		return nil, protocol.Wrap(protocol.CodeIO, err, "渲染详情卡片失败")
	}

	return &RenderResult{
		OutputPath: out,
		Width:      imageWidth(out),
		Height:     imageHeight(out),
		ElapsedMS:  time.Since(start).Milliseconds(),
	}, nil
}

// RenderAlbumCommentImage 渲染评论区图片。
func (s *Service) RenderAlbumCommentImage(ctx context.Context, prog ProgressFunc, req RenderRequest) (*RenderResult, error) {
	start := time.Now()
	cfg := s.Config()

	cli, cfg2, err := s.ensureClient(ctx, prog)
	if err != nil {
		return nil, err
	}
	if cfg2.CDNHost != "" {
		cfg.CDNHost = cfg2.CDNHost
	}

	list, err := s.AlbumComments(ctx, prog, AlbumCommentsRequest{AID: req.AID, AllPages: true})
	if err != nil {
		return nil, err
	}

	// 标题用作品名；作品名拿不到不是致命问题，退回「作品 <aid>」。
	title := "作品 " + itoa(req.AID) + " 评论区"
	if detail, err := cli.AlbumDetail(ctx, req.AID); err == nil && detail.Name != "" {
		title = trimUnicodeSpace(detail.Name) + " · 评论区"
	}

	comments := make([]utils.Comment, 0, len(list.Comments))
	// 表情图先批量抓完再渲染：排版层没有网络，而且同一张贴纸会被很多人用，
	// 去重后一次性下完比"边排版边下"少打很多次请求。
	inline := s.fetchInlineImages(ctx, cfg, list.Comments, prog)
	for i := range list.Comments {
		comments = append(comments, s.toComment(ctx, cfg, &list.Comments[i], inline))
	}

	note := "共 " + itoa(list.Total) + " 条"
	if len(list.Comments) < list.Total {
		note = "已加载 " + itoa(len(list.Comments)) + " / 共 " + itoa(list.Total) + " 条"
	}

	out := req.OutputPath
	if out == "" {
		out = filepath.Join(cfg.OutputDir, itoa(req.AID)+"_comments.png")
	}

	prog.report(Progress{Stage: "render", Message: "正在渲染评论图"})
	if err := utils.RenderCommentPage(title, note, comments, out); err != nil {
		return nil, protocol.Wrap(protocol.CodeIO, err, "渲染评论图失败")
	}

	// 宽高如实回填：宿主靠它决定怎么摆这张长图（评论图可能有几千像素高）。
	return &RenderResult{
		OutputPath: out,
		Width:      imageWidth(out),
		Height:     imageHeight(out),
		ElapsedMS:  time.Since(start).Milliseconds(),
	}, nil
}

// toComment 把接口的评论条目转成渲染层入参（楼中楼递归处理）。
//
// 头像与勋章是「有则更佳」的资源：下载失败时留空，渲染层会退化成
// 首字头像 / 不画勋章，不影响整张图产出。
//
// inline 是本次渲染预抓好的表情图（URL → 本地路径），整棵树共享同一份：
// 渲染层按正文里出现的地址去查，用不上就自然忽略。
func (s *Service) toComment(
	ctx context.Context, cfg *config.Config, raw *client.List, inline map[string]string,
) utils.Comment {
	c := utils.Comment{
		Nickname:     trimUnicodeSpace(raw.Nickname),
		Content:      raw.Content,
		AddTime:      raw.Addtime,
		UID:          raw.UID,
		Likes:        atoiSafe(raw.Likes),
		Level:        raw.Expinfo.Level,
		LevelName:    trimUnicodeSpace(raw.Expinfo.LevelName),
		InlineImages: inline,
	}

	if c.Nickname == "" {
		c.Nickname = trimUnicodeSpace(raw.Username)
	}

	if cfg.CDNHost != "" {
		c.AvatarPath = s.fetchAsset(ctx, cfg, client.BuildUserAvatarURL(cfg.CDNHost, raw.Photo), "avatar", raw.CID)
		for _, b := range raw.Expinfo.Badges {
			if p := s.fetchAsset(ctx, cfg, client.BuildBadgeImageURL(cfg.CDNHost, b.Content), "badge", b.Id); p != "" {
				c.Badges = append(c.Badges, p)
			}
		}
	}

	if raw.Replys != nil {
		for i := range *raw.Replys {
			c.Replies = append(c.Replies, s.toComment(ctx, cfg, replyToList(&(*raw.Replys)[i]), inline))
		}
	}
	return c
}

// replyToList 把楼中楼的 Reply 结构适配成 List，让 toComment 只认一种形状。
func replyToList(r *client.Reply) *client.List {
	return &client.List{
		AID:      "",
		CID:      r.CID,
		UID:      r.UID,
		Username: r.Username,
		Nickname: r.Nickname,
		Likes:    r.Likes,
		Addtime:  r.Addtime,
		Photo:    r.Photo,
		Content:  r.Content,
		Expinfo:  r.Expinfo,
	}
}

// fetchAsset 把一张远程小图（头像/勋章）下到本地缓存，返回本地路径。//
// 失败返回空串而不是错误：这类资源缺失只影响观感，不该让整个渲染失败。
// 下载器自带的图片签名校验对 gif/png 都放行，所以头像（gif）与勋章（png）
// 不需要为它们放宽校验——放宽反而会让「CDN 返回 HTML 错误页」被当成图片存下来。
func (s *Service) fetchAsset(ctx context.Context, cfg *config.Config, rawURL, kind, id string) string {
	if rawURL == "" || id == "" {
		return ""
	}
	dst := filepath.Join(cacheDir(cfg), kind+"_"+sanitizeOr(id, "x")+mediaExt(rawURL))

	if err := s.downloaderFor(cfg).Download(ctx, rawURL, dst); err != nil {
		return ""
	}
	return dst
}

// chapterNames 提取章节名列表。
func chapterNames(chs []ChapterBrief) []string {
	if len(chs) == 0 {
		return nil
	}
	out := make([]string, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch.Name)
	}
	return out
}

// readingIndex 返回「正在阅读的章节序号」（1-based，0 表示没有选中章节）。
//
// 判定方式很直白：请求的 id 命中哪个章节，就高亮哪个。
// 用户从作品主 id 进来时它不命中任何章节，自然就是 0，不需要额外区分。
func readingIndex(chs []ChapterBrief, requestedID int) int {
	if requestedID <= 0 {
		return 0
	}
	for i, ch := range chs {
		if ch.ID == requestedID {
			return i + 1
		}
	}
	return 0
}

// cacheDir 返回本次运行共享的资源缓存目录（封面、头像、勋章）。
func cacheDir(cfg *config.Config) string {
	out := cfg.OutputDir
	if out == "" {
		out = "output"
	}
	return filepath.Join(out, ".cache")
}

// sanitizeOr 清洗文件名，结果为空时返回 fallback。
func sanitizeOr(s, fallback string) string {
	if v := sanitizeFileName(s); v != "" {
		return v
	}
	return fallback
}

// imageWidth / imageHeight 读回渲染结果的尺寸，读不到时返回 0。
//
// 尺寸只是回给宿主做展示用，读失败不该让「渲染成功」变成失败。
//
// 名字不叫 cardWidth/cardHeight：这张长图既可能是详情卡（2160x1280），
// 也可能是评论图（1920x 好几千），按"卡"命名会误导后来人。
func imageWidth(path string) int  { return imageDim(path, true) }
func imageHeight(path string) int { return imageDim(path, false) }
