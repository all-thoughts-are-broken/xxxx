package service

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// downloadChapter 完成单章的「下载 → 还原 → 合成 PDF」。
//
// 目录布局（albumID 与 chapterID 都取自接口返回的真实 id）：
//
//	<download_dir>/<albumID>/<chapterID>/00001.webp   原始混淆图
//	<output_dir>/<albumID>/<chapterID>/00001.jpg      还原后的图
//	<output_dir>/<albumID>/<章节名>.pdf                单章 PDF
func (s *Service) downloadChapter(
	ctx context.Context,
	prog ProgressFunc,
	cfg *config.Config,
	downloader *utils.Downloader,
	cli *client.Client,
	albumID int,
	ch ChapterBrief,
	restoreExt string,
	chIdx, chTotal int,
	req DownloadAlbumRequest,
) (*ChapterResult, error) {
	page, err := cli.ComicRead(ctx, ch.ID)
	if err != nil {
		return nil, protocol.Wrap(protocol.CodeNetwork, err, "获取章节 %d 的阅读页失败", ch.ID)
	}

	chapterID := page.Id
	if chapterID <= 0 {
		chapterID = ch.ID
	}
	chName := trimUnicodeSpace(ch.Name)
	if chName == "" {
		chName = trimUnicodeSpace(page.Name)
	}
	if chName == "" {
		chName = "第" + itoa(chIdx+1) + "章"
	}

	// scramble_id 优先取阅读页返回的（每章可能不同），取不到才退回配置值。
	scrambleID := page.ScrambleID()
	if scrambleID <= 0 {
		scrambleID = cfg.ScrambleID
	}

	rawDir := filepath.Join(cfg.DownloadDir, itoa(albumID), itoa(chapterID))
	imageDir := filepath.Join(cfg.OutputDir, itoa(albumID), itoa(chapterID))

	res := &ChapterResult{
		ID:       chapterID,
		Name:     chName,
		Images:   len(page.Images),
		RawDir:   rawDir,
		ImageDir: imageDir,
		PDFBytes: -1,
	}

	plan := buildPagePlan(page)
	if len(plan) == 0 {
		return nil, protocol.Errorf(protocol.CodeNotFound, "章节 %d 的阅读页没有任何有效图片", chapterID)
	}

	chapExtra := map[string]any{
		"chapter_id":    chapterID,
		"chapter":       chName,
		"chapter_index": chIdx + 1,
		"chapter_total": chTotal,
	}

	// ---- 阶段 1：下载原始（混淆）图 ----
	//
	// 每页都带一组备用直链：接口下发的图片域名可能已经下线，而同一路径换个
	// 镜像域名就是好的（见 alternateImageHosts 的说明）。没有备用链的话，
	// 一旦摊上坏域名，整章会退化成占位图。
	altHosts := alternateImageHosts(cfg.CDNHost, page)

	dlTasks := make([]utils.AlternateDownloadTask, 0, len(plan))
	for _, p := range plan {
		dlTasks = append(dlTasks, utils.AlternateDownloadTask{
			Url:        p.url,
			Dist:       filepath.Join(rawDir, p.rawName),
			Alternates: alternateURLs(p.url, altHosts),
		})
	}

	dlResults := downloader.BatchWithAlternates(ctx, dlTasks, cfg.EffectiveWorkers(),
		func(done, total int, r utils.BatchDownloadResult) {
			extra := copyExtra(chapExtra)
			extra["file"] = filepath.Base(r.Dist)
			prog.report(Progress{
				Stage:   "download",
				Done:    done,
				Total:   total,
				Message: "下载 " + filepath.Base(r.Dist),
				Extra:   extra,
			})
		})

	// 记下每页的失败原因；没下下来的页不参与还原，最后统一补占位图。
	downloadErr := make(map[int]string, 4)
	for i, r := range dlResults {
		if r.OK() {
			continue
		}
		downloadErr[plan[i].page] = msgOf(r.Err)
	}

	// ---- 阶段 2：还原 ----
	//
	// GIF 例外：JS 端 scramble_image 对 .gif 直接跳过（"GIF和舊漫沒切不用還原"），
	// 这里保持一致，并且原样转存成 .gif —— gofpdf 原生支持 gif，
	// 强行转成 JPEG 反而会丢帧、体积还更大。
	var (
		restoreTasks []utils.DecodeAndSaveTask
		restorePlans []pagePlan
	)

	for _, p := range plan {
		if _, bad := downloadErr[p.page]; bad {
			continue
		}
		src := filepath.Join(rawDir, p.rawName)
		if !fileExists(src) {
			// 下载报成功但文件不在（被 skip_existing 跳过，或落盘中断）。
			downloadErr[p.page] = "原始文件缺失"
			continue
		}

		dst := filepath.Join(imageDir, p.pageName+p.outExt(restoreExt))
		if p.isGIF() {
			if err := copyFile(src, dst); err != nil {
				addFailure(res, ImageFailure{Page: p.page, URL: p.url, Stage: "restore", Error: msgOf(err)})
			}
			continue
		}

		restoreTasks = append(restoreTasks, utils.DecodeAndSaveTask{
			ImgSrcPath:      src,
			DecodedSavePath: dst,
			Name:            p.pageName,
		})
		restorePlans = append(restorePlans, p)
	}

	if len(restoreTasks) > 0 {
		// 关键：CountImageSegments 里的 aid 必须是**阅读页自身的 id**
		// （对应 JS 端 get_num(readList.id, img.alt)），不是作品号 —— 传错会还原出花屏。
		restoreResults := utils.BatchDecodeAndSaveWithProgress(
			scrambleID, chapterID, restoreTasks, cfg.EffectiveWorkers(),
			func(done, total int, r utils.DecodeAndSaveResult) {
				extra := copyExtra(chapExtra)
				extra["file"] = filepath.Base(r.ImgSrcPath)
				extra["segments"] = r.Segments
				prog.report(Progress{
					Stage:   "restore",
					Done:    done,
					Total:   total,
					Message: "还原 " + filepath.Base(r.ImgSrcPath),
					Extra:   extra,
				})
			})

		// 结果与入参同序，所以用 restorePlans[i] 反查是安全的。
		for i, r := range restoreResults {
			if r.OK() {
				continue
			}
			addFailure(res, ImageFailure{
				Page:  restorePlans[i].page,
				URL:   restorePlans[i].url,
				Stage: "restore",
				Error: msgOf(r.Err),
			})
		}
	}

	// 补齐：任何一页缺文件都写占位图，保证页数页序与真实章节一致。
	for _, p := range plan {
		dst := filepath.Join(imageDir, p.pageName+p.outExt(restoreExt))
		if fileExists(dst) {
			continue
		}
		reason := downloadErr[p.page]
		if reason == "" {
			reason = "还原失败"
		}
		if err := utils.WritePlaceholder(dst, 1000, 1414, "第 "+itoa(p.page)+" 页不可用\n"+reason); err != nil {
			// 占位图都写不出来（磁盘满/权限），这页只能缺了。
			addFailure(res, ImageFailure{Page: p.page, URL: p.url, Stage: "restore", Error: reason + "；占位图写入失败: " + msgOf(err)})
			continue
		}
		addFailure(res, ImageFailure{Page: p.page, URL: p.url, Stage: "download", Error: reason})
	}

	res.Failed = len(res.Failures) + res.DroppedFailures
	if req.SkipPDF {
		return res, nil
	}

	// ---- 阶段 3：合成 PDF ----
	images := make([]string, 0, len(plan))
	for _, p := range plan {
		images = append(images, filepath.Join(imageDir, p.pageName+p.outExt(restoreExt)))
	}

	pdfPath := filepath.Join(cfg.OutputDir, itoa(albumID), safeChapterFileName(chName, chapterID)+".pdf")
	prog.report(Progress{
		Stage:   "pdf",
		Total:   len(images),
		Message: "合成《" + chName + "》PDF（" + itoa(len(images)) + " 页）",
		Extra:   copyExtra(chapExtra),
	})

	if err := utils.GeneratePdfFromImages(images, pdfPath, utils.PDFOptions{
		Chapter:          chName,
		MaxPageHeight:    cfg.PDFMaxPageHeight,
		Layout:           cfg.PDFLayout,
		PerImageBookmark: cfg.PDFPerImageBookmark,
	}); err != nil {
		return res, protocol.Wrap(protocol.CodeIO, err, "合成章节 %d 的 PDF 失败", chapterID)
	}

	if st, err := os.Stat(pdfPath); err == nil {
		res.PDFBytes = st.Size()
	}
	res.PDFPath = pdfPath
	return res, nil
}

// pagePlan 描述一页图：URL、原始文件名、还原后文件名。
//
// 原始文件名必须沿用服务端的命名（00001.webp）：段数计算依赖
// md5(aid + 不含扩展名的文件名)，改名会导致还原段数算错。
type pagePlan struct {
	page     int
	url      string
	rawName  string
	pageName string // 不含扩展名的文件名，参与段数计算
}

// isGIF 报告源图是否是 GIF（GIF 不参与切图还原）。
func (p pagePlan) isGIF() bool {
	return strings.EqualFold(filepath.Ext(p.rawName), ".gif")
}

// outExt 返回该页还原结果应使用的扩展名。
//
// 默认用调用方指定的格式（jpg/png）；源是 GIF 时保持 gif，
// 因为「不还原、原样转存」的语义要求不改格式。
func (p pagePlan) outExt(defaultExt string) string {
	if p.isGIF() {
		return ".gif"
	}
	return defaultExt
}

// buildPagePlan 由阅读页响应生成逐页计划。
func buildPagePlan(page *client.ReadPageResult) []pagePlan {
	out := make([]pagePlan, 0, len(page.Images))
	seen := make(map[string]int, len(page.Images))

	for i, img := range page.Images {
		u := strings.TrimSpace(img.Image)
		if u == "" {
			continue
		}

		pageNo := img.Page
		if pageNo <= 0 {
			pageNo = i + 1
		}

		rawName := rawNameFromURL(u)
		if rawName == "" {
			rawName = client.PadPage(pageNo) + ".webp"
		}
		pageName := strings.TrimSuffix(rawName, filepath.Ext(rawName))
		if pageName == "" {
			pageName = client.PadPage(pageNo)
		}

		// 服务端偶发重复页时的兜底：加后缀避免互相覆盖。
		key := strings.ToLower(rawName)
		if n, dup := seen[key]; dup {
			seen[key] = n + 1
			suffix := "_" + itoa(n+1)
			rawName = pageName + suffix + filepath.Ext(rawName)
			pageName += suffix
		} else {
			seen[key] = 1
		}

		out = append(out, pagePlan{page: pageNo, url: u, rawName: rawName, pageName: pageName})
	}
	return out
}

// rawNameFromURL 从图片直链里取出文件名（含扩展名，去掉 query）。
func rawNameFromURL(rawURL string) string {
	s := rawURL
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexAny(s, "/\\"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	// Windows 非法字符出现在文件名里说明 URL 不对，交给调用方兜底命名。
	if s == "" || s == "." || strings.ContainsAny(s, `<>:"|*?`) {
		return ""
	}
	return s
}

// copyFile 复制文件（GIF 原样转存用）。
//
// 同样先写 .part 再改名：这个函数的产物也是「还原结果」，半截图一旦落盘就会
// 被当成有效输入静默流进 PDF。与下载链路、还原链路保持同一套纪律。
func copyFile(src, dst string) error {
	if err := utils.EnsureDir(filepath.Dir(dst)); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// fileExists 报告路径存在且不是目录。
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// addFailure 追加一条失败记录（最多保留 maxReportedFailures 条）。
//
// 超出上限后不再逐条记录，而是在最后一条上累积计数 ——
// 一本 200 页的漫画如果整本站都挂了，逐条回传会把结果帧撑到几百 KB。
func addFailure(res *ChapterResult, f ImageFailure) {
	if len(res.Failures) >= maxReportedFailures {
		res.DroppedFailures++
		return
	}
	res.Failures = append(res.Failures, f)
}

// msgOf 把 error 转成字符串，nil 时返回空串。
func msgOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- 图片 CDN 备用域名 ----

// alternateImageHosts 收集可以替用的图片 CDN 域名，按优先级排列。
//
// 背景：接口下发的图片直链域名是会轮换的，轮换过程中会出现**已经下线**的域名 ——
// 实测 tencent.jmdanjonproxy.xyz、cdn-msp2.jmdanjonproxy.vip 这类连 TLS 都握不上手
// （connection reset），而同一路径在 cdn-msp2.jmapiproxy3.cc 上是好的，
// 而且字节数完全一致。死盯坏域名重试没有意义，换域名才有用。
//
// 候选来源全部来自服务端自己的信息，不硬编码域名：
//  1. preferred —— 探活时学到的 CDN 域名，已被验证可用并写回了配置
//  2. 本章直链里出现过的其它域名 —— 服务端偶尔会在同一个响应里混用多个镜像
func alternateImageHosts(preferred string, page *client.ReadPageResult) []string {
	out := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		out = append(out, host)
	}

	add(preferred)
	if page != nil {
		for _, img := range page.Images {
			add(client.HostFromURL(img.Image))
		}
	}
	return out
}

// alternateURLs 为一条图片直链生成备用直链：**路径不变，只换域名**。
//
// 图片路径（/media/photos/<aid>/<page:05>.webp）与域名无关，实测同一路径在多个
// 镜像上返回完全相同的字节；?t= 参数不是必须的，原样带着也无害。
//
// 与主链同域名的候选会被跳过（去掉自重复），失败时返回 nil。
func alternateURLs(rawURL string, hosts []string) []string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || len(hosts) == 0 {
		return nil
	}

	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if strings.EqualFold(strings.TrimSpace(host), u.Host) {
			continue
		}
		alt := *u
		alt.Host = strings.TrimSpace(host)
		out = append(out, alt.String())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// copyExtra 浅拷贝一个进度附加信息 map，避免多个事件的 Extra 共享同一份底层数据。
func copyExtra(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}
