package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// resolveChapters 决定这次要下载哪些章节，并顺带拿到作品名与作品号。
//
// 三种模式：
//
//	req.ChapterIDs 非空  → 就用它（顺序即下载顺序）
//	req.AllChapters      → 用详情页列出的全部章节
//	其它                 → 只下 req.ID 这一个（可能是作品主 id，也可能是章节号）
//
// 返回的 albumID 用于组织输出目录：优先用详情里的真实作品号，
// 这样「传章节号下载」和「传作品号下载同一章」会落到同一个目录，
// 不会因为入口不同而把同一本漫画存成两份。
func (s *Service) resolveChapters(
	ctx context.Context, cli *client.Client, req DownloadAlbumRequest,
) (targets []ChapterBrief, albumName string, albumID int, err error) {
	albumID = req.ID

	// 只在需要章节列表（或需要作品名做目录/文件名）时才请求详情接口。
	needDetail := req.AllChapters || len(req.ChapterIDs) > 0
	var detail *client.AlbumDetailResult

	if needDetail {
		detail, err = cli.AlbumDetail(ctx, req.ID)
		if err != nil {
			return nil, "", 0, protocol.Wrap(protocol.CodeNotFound, err, "获取作品 %d 详情失败", req.ID)
		}
		albumID = detail.Id
		albumName = trimUnicodeSpace(detail.Name)
	}

	// 显式指定的章节优先。
	if len(req.ChapterIDs) > 0 {
		for _, id := range req.ChapterIDs {
			if id <= 0 {
				continue
			}
			targets = append(targets, ChapterBrief{ID: id, Name: s.chapterNameOf(detail, id)})
		}
	} else if req.AllChapters {
		if detail != nil {
			for _, ch := range detailToChapters(detail) {
				targets = append(targets, ch)
			}
		}
	} else {
		// 单章：不请求详情接口，直接用请求的 id。
		targets = append(targets, ChapterBrief{ID: req.ID})
	}

	if req.MaxChapters > 0 && len(targets) > req.MaxChapters {
		targets = targets[:req.MaxChapters]
	}
	if albumName == "" {
		albumName = ""
	}
	return targets, albumName, albumID, nil
}

// chapterNameOf 在详情里查章节名；查不到返回空串（上层会补「第 N 章」）。
func (s *Service) chapterNameOf(detail *client.AlbumDetailResult, id int) string {
	if detail == nil {
		return ""
	}
	for _, ch := range detailToChapters(detail) {
		if ch.ID == id {
			return ch.Name
		}
	}
	return ""
}

// detailToChapters 从详情响应里抽出章节列表（归一化 + 排序）。
func detailToChapters(detail *client.AlbumDetailResult) []ChapterBrief {
	if detail == nil {
		return nil
	}
	out := make([]ChapterBrief, 0, len(detail.Series))
	for _, ch := range detail.Series {
		chID := atoiSafe(ch.Id)
		if chID <= 0 {
			continue
		}
		chSort := atoiSafe(ch.Sort)
		name := trimUnicodeSpace(ch.Name)
		if name == "" && chSort > 0 {
			name = "第" + itoa(chSort) + "章"
		}
		out = append(out, ChapterBrief{ID: chID, Name: name, Sort: chSort})
	}
	sortChapters(out)
	return out
}

// applyDirOverrides 把请求里的目录覆盖应用到配置副本上。
//
// 用副本而不是改全局配置：目录是「本次调用」的参数，
// 让它污染后续请求会让宿主很难推理自己的调用产生了什么副作用。
func (s *Service) applyDirOverrides(cfg *config.Config, req *DownloadAlbumRequest) *config.Config {
	cp := *cfg
	if strings.TrimSpace(req.RawDir) != "" {
		cp.DownloadDir = strings.TrimSpace(req.RawDir)
	}
	if strings.TrimSpace(req.OutputDir) != "" {
		cp.OutputDir = strings.TrimSpace(req.OutputDir)
	}
	if cp.DownloadDir == "" {
		cp.DownloadDir = "download"
	}
	if cp.OutputDir == "" {
		cp.OutputDir = "output"
	}
	return &cp
}

// normalizeRestoreExt 归一化还原格式，只接受 jpg/jpeg/png。
//
// 允许 jpeg 是为了让配置写起来自然（.jpeg 与 .jpg 是同一格式）。
func normalizeRestoreExt(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "jpg", "jpeg":
		return ".jpg"
	case "png":
		return ".png"
	default:
		return ".jpg"
	}
}

// encryptInPlace 就地把 PDF 加密。
//
// 先写到临时文件再替换原文件：pdfcpu 的 EncryptFile 要求输入输出是不同文件，
// 直接 input==output 会失败；而且分两步走能保证「加密失败时原文件还在」。
func encryptInPlace(pdfPath, userPassword, ownerPassword string) (string, error) {
	tmp := pdfPath + ".enc"
	defer os.Remove(tmp) // 成功路径下已被 rename 掉，这里只清理失败残留

	if err := utils.SetPDFPasswordWithOwner(pdfPath, tmp, userPassword, ownerPassword); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, pdfPath); err != nil {
		return "", protocol.Wrap(protocol.CodeIO, err, "替换加密后的 PDF %s 失败", pdfPath)
	}
	return pdfPath, nil
}

// removeRestoredImages 删除本次还原出来的图片。
//
// 只删「结果里明确列出的那些文件」，不做目录级递归删除——
// 目录里可能还有用户自己放的东西。
func removeRestoredImages(chapters []ChapterResult) {
	for _, ch := range chapters {
		if ch.ImageDir == "" {
			continue
		}
		removeDirIfOnlyOurs(ch.ImageDir)
	}
}

// removeRawImages 删除服务端原始（混淆）图片。
func removeRawImages(chapters []ChapterResult) {
	for _, ch := range chapters {
		if ch.RawDir == "" {
			continue
		}
		removeDirIfOnlyOurs(ch.RawDir)
	}
}

// clearRawDirs 清空结果里的 RawDir，避免把已删除的目录回报给宿主。
func clearRawDirs(chapters []ChapterResult) []ChapterResult {
	for i := range chapters {
		chapters[i].RawDir = ""
	}
	return chapters
}

// clearImageDirs 清空结果里的 ImageDir（同上，已删除才清）。
func clearImageDirs(chapters []ChapterResult) []ChapterResult {
	for i := range chapters {
		chapters[i].ImageDir = ""
	}
	return chapters
}

// removeDirIfOnlyOurs 删除一个「本工具自己创建」的目录。
//
// 保守做法：只删除目录里扩展名属于本流程产物（图片）的文件，
// 然后再尝试用 os.Remove 删掉空目录（非空则自然失败，不会误删用户文件）。
func removeDirIfOnlyOurs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			return // 有子目录：不是纯产物目录，直接放弃
		}
		if !utils.IsImageFile(e.Name()) {
			return // 有非图片文件：可能混了用户文件，放弃
		}
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	_ = os.Remove(dir)
}
