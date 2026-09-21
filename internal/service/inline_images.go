package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// ---------------- 评论正文里的表情图 ----------------
//
// 评论里的「表情」其实是两类图片，都不是字体问题：
//   - 服务端表情面板插入的贴纸：正文里直接就是
//     `<img src="https://www.cdnbea.net/media/emoji/<sha1>.png" alt="惊喜">`
//   - 用户手打的 Unicode 码位（😋 / ❤️ / 👍🏽），映射到 Twemoji 的 72x72 PNG
//
// 渲染层是纯计算的（没有网络），所以这里在渲染前把整棵评论树里用到的表情图
// 一次抓完、落到 .cache/ 下，再以「直链 → 本地路径」的形式交给渲染层。

// inlineImageWorkers 并行抓取表情图的协程数。
//
// 表情图都很小（Twemoji 72x72 不到 1KB，贴纸几 KB），瓶颈在往返延迟，
// 所以多开一点；连接池由 downloaderFor 复用。
const inlineImageWorkers = 8

// collectInlineImages 遍历整棵评论树，收集正文引用到的内联图（按出现顺序、URL 去重）。
//
// 分词规则与渲染层共用 utils.CollectInlineImages，避免「抓的」与「画的」分叉。
// 楼中楼只下钻一层：接口的 replys 里没有更深的结构，渲染层也只画一层。
func collectInlineImages(comments []client.List) []utils.InlineImage {
	var (
		out  []utils.InlineImage
		seen = map[string]bool{}
	)
	add := func(content string) {
		for _, im := range utils.CollectInlineImages(content) {
			if im.URL == "" || seen[im.URL] {
				continue
			}
			seen[im.URL] = true
			out = append(out, im)
		}
	}

	for i := range comments {
		add(comments[i].Content)
		if comments[i].Replys == nil {
			continue
		}
		for _, r := range *comments[i].Replys {
			add(r.Content)
		}
	}
	return out
}

// fetchInlineImages 抓取评论正文里的表情图，返回「直链 → 本地文件」映射。
//
// 抓不到的图不报错也不中断：渲染层会把它退化成 [alt] / 原始码位文字。
// 反过来说，**绝不能因为抓不到就当这张图不存在** —— 纯表情评论会因此被
// 判成「这条评论没有内容」，这正是要修的那个 bug。
func (s *Service) fetchInlineImages(
	ctx context.Context, cfg *config.Config, comments []client.List, prog ProgressFunc,
) map[string]string {
	items := collectInlineImages(comments)
	if len(items) == 0 {
		return nil
	}

	tasks := make([]utils.AlternateDownloadTask, 0, len(items))
	for _, im := range items {
		tasks = append(tasks, utils.AlternateDownloadTask{
			Url:        im.URL,
			Dist:       filepath.Join(cacheDir(cfg), "emoji_"+urlHash(im.URL)+mediaExt(im.URL)),
			Alternates: inlineAlternates(cfg, im),
		})
	}

	prog.report(Progress{Stage: "render", Message: "正在抓取表情图 " + itoa(len(tasks)) + " 张"})
	results := s.downloaderFor(cfg).BatchWithAlternates(ctx, tasks, inlineImageWorkers, nil)

	out := make(map[string]string, len(results))
	failed := 0
	for i, r := range results {
		if r.Err != nil {
			failed++
			continue
		}
		out[items[i].URL] = r.Dist
	}
	if failed > 0 {
		prog.report(Progress{
			Stage:   "render",
			Message: "有 " + itoa(failed) + " 张表情图没抓到，改画占位文字",
		})
	}
	return out
}

// inlineAlternates 返回表情图的备用直链。
//
// 只有服务端贴纸才换域名：那类图与漫画图片同源，路径与域名无关，实测换个镜像
// 域名就能拿到同样的字节。Twemoji 在第三方 CDN 上，硬套镜像只会白等一轮失败。
func inlineAlternates(cfg *config.Config, im utils.InlineImage) []string {
	if !im.Sticker || cfg.CDNHost == "" {
		return nil
	}
	return alternateURLs(im.URL, []string{cfg.CDNHost})
}

// urlHash 给图片地址算一个稳定短名（当缓存文件名用）。
//
// 用地址本身而不是它在评论里的序号：同一张贴纸在多次渲染之间复用同一个文件，
// 下载器的 skip_existing 才能真的省下请求。
func urlHash(u string) string {
	sum := md5.Sum([]byte(u))
	return hex.EncodeToString(sum[:])
}

// mediaExt 取图片扩展名，取不到或明显不是扩展名时按 .png。
func mediaExt(rawURL string) string {
	ext := filepath.Ext(strings.SplitN(rawURL, "?", 2)[0])
	if ext == "" || len(ext) > 5 {
		return ".png"
	}
	return ext
}
