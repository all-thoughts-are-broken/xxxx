package service

import (
	"context"
	"path/filepath"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// DownloadAlbumRequest 是「下载整本/整章」的请求。
type DownloadAlbumRequest struct {
	// ID 作品号或章节号。
	ID int `json:"id"`

	// AllChapters 为 true 时下载详情页列出的全部章节。
	AllChapters bool `json:"all_chapters"`
	// ChapterIDs 指定要下载的章节号（与 AllChapters 同时给出时以 ChapterIDs 为准）。
	ChapterIDs []int `json:"chapter_ids"`
	// MaxChapters 限制最多下载多少章；<=0 不限。
	MaxChapters int `json:"max_chapters"`

	// MergeChapters 为 true 时把所有章节合成一本 PDF（否则每章一个 PDF）。
	MergeChapters bool `json:"merge_chapters"`

	// RawDir / OutputDir 覆盖配置里的目录；为空则用配置值。
	RawDir    string `json:"raw_dir"`
	OutputDir string `json:"output_dir"`
	// OutputPDF 合并后的 PDF 路径；为空时自动命名。
	OutputPDF string `json:"output_pdf"`

	// RestoreFormat 还原后的图片格式（"jpg" 或 "png"），默认 jpg。
	//
	// 默认 JPEG 是有原因的：PDF 库不支持 webp，统一落成 JPEG 才能直通合成。
	RestoreFormat string `json:"restore_format"`

	// SkipPDF 只下图片、不合成 PDF。
	SkipPDF bool `json:"skip_pdf"`
	// KeepRaw 保留服务端原始（混淆）图片。
	KeepRaw bool `json:"keep_raw"`
	// SkipImages 合成 PDF 后删除还原出来的图片（只要 PDF）。
	SkipImages bool `json:"skip_images"`
}

// ImageFailure 记录一张图的失败。
type ImageFailure struct {
	// Page 页码（1-based）。只有下载链路有页码概念；纯还原本地目录时省略，
	// 避免回传一个误导性的 0。
	Page  int    `json:"page,omitempty"`
	URL   string `json:"url"`
	Stage string `json:"stage"` // download / restore
	Error string `json:"error"`
}

// ChapterResult 是单章的下载结果。
type ChapterResult struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Images   int    `json:"images"`
	Failed   int    `json:"failed"`
	RawDir   string `json:"raw_dir,omitempty"`
	ImageDir string `json:"image_dir,omitempty"`
	PDFPath  string `json:"pdf_path,omitempty"`
	// PDFBytes PDF 文件大小；-1 表示未生成。
	PDFBytes int64 `json:"pdf_bytes"`
	// Failures 最多回传前 maxReportedFailures 条失败明细，避免结果帧被上千条错误撑爆。
	Failures []ImageFailure `json:"failures,omitempty"`
	// DroppedFailures 记录因超过上限而未列出的失败条数。
	DroppedFailures int `json:"dropped_failures,omitempty"`
	// Error 整章失败时的原因（单张图失败记在 Failures 里，不算整章失败）。
	Error string `json:"error,omitempty"`
}

// DownloadAlbumResult 是整次下载的结果。
type DownloadAlbumResult struct {
	AID      int             `json:"aid"`
	Name     string          `json:"name"`
	Chapters []ChapterResult `json:"chapters"`
	// PDFPath 最终交付的 PDF（合并后的，或只有一章时的那个）。
	PDFPath string `json:"pdf_path,omitempty"`
	// PDFPaths 每个章节各自的 PDF 路径。
	PDFPaths []string `json:"pdf_paths,omitempty"`
	// Encrypted 报告最终 PDF 是否已加密。
	Encrypted   bool  `json:"encrypted"`
	TotalPages  int   `json:"total_pages"`
	FailedTotal int   `json:"failed_total"`
	ElapsedMS   int64 `json:"elapsed_ms"`
}

// maxReportedFailures 是每章回传的失败明细上限。
const maxReportedFailures = 20

// DownloadAlbum 执行完整的「下载 → 还原 → 合成 PDF」流水线。
//
// 阶段划分（每阶段都有独立进度事件）：
//
//	probe    线路探活（仅当配置里没有 base_url 时）
//	download 拉取服务端原始（混淆）图片
//	restore  还原切图并输出 JPEG
//	pdf      逐章合成 PDF
//	merge    多章合并
//	encrypt  按配置加密 PDF
//
// 单张图失败不会中断整本下载：会在对应位置插入占位图，保证 PDF 的
// 页数与页序与真实章节一致（否则读者看到的页码会整体错位）。
func (s *Service) DownloadAlbum(ctx context.Context, prog ProgressFunc, req DownloadAlbumRequest) (*DownloadAlbumResult, error) {
	start := time.Now()

	if req.ID <= 0 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "作品号非法: %d", req.ID)
	}
	if req.MergeChapters && req.SkipPDF {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "merge_chapters 与 skip_pdf 不能同时为 true")
	}

	cli, cfg, err := s.ensureClient(ctx, prog)
	if err != nil {
		return nil, err
	}
	cfg = s.applyDirOverrides(cfg, &req)

	targets, albumName, albumID, err := s.resolveChapters(ctx, cli, req)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, protocol.Errorf(protocol.CodeNotFound, "作品 %d 没有可下载的章节", req.ID)
	}

	res := &DownloadAlbumResult{AID: albumID, Name: albumName}

	downloader := s.downloaderFor(cfg)
	restoreExt := normalizeRestoreExt(req.RestoreFormat)

	for i, ch := range targets {
		if err := ctx.Err(); err != nil {
			return res, protocol.Wrap(protocol.CodeCanceled, err,
				"下载被中断，已完成 %d/%d 章", i, len(targets))
		}

		chName := ch.Name
		if chName == "" {
			chName = "第" + itoa(i+1) + "章"
		}
		prog.report(Progress{
			Stage:   "chapter",
			Done:    i,
			Total:   len(targets),
			Message: "开始处理《" + chName + "》",
			Extra:   map[string]any{"chapter_id": ch.ID, "chapter_index": i + 1},
		})

		cr, err := s.downloadChapter(ctx, prog, cfg, downloader, cli, albumID, ch, restoreExt, i, len(targets), req)
		if err != nil {
			// 单章失败：记录后继续下一章。整本里某一章挂了不该让用户前功尽弃。
			prog.report(Progress{
				Stage:   "chapter",
				Done:    i + 1,
				Total:   len(targets),
				Message: "《" + chName + "》失败: " + err.Error(),
				Extra:   map[string]any{"chapter_id": ch.ID, "error": err.Error()},
			})
			res.Chapters = append(res.Chapters, ChapterResult{
				ID: ch.ID, Name: chName, PDFBytes: -1, Error: err.Error(),
			})
			continue
		}

		res.Chapters = append(res.Chapters, *cr)
		res.TotalPages += cr.Images
		res.FailedTotal += cr.Failed
		if cr.PDFPath != "" {
			res.PDFPaths = append(res.PDFPaths, cr.PDFPath)
		}
	}

	// ---- 合并 / 加密 / 清理 ----
	//
	// 这里按「生成了几个章节 PDF」分支。全部章节都失败时 res.PDFPaths 为空，
	// 但除了 skip_pdf 的情况下这属于真正的失败，要显式报错，
	// 否则宿主只会拿到一个 chapters 里全是 error 的空结果。
	switch {
	case len(res.PDFPaths) == 0:
		if !req.SkipPDF {
			return res, protocol.Errorf(protocol.CodeInternal,
				"作品 %d 的 %d 个章节全部下载失败", req.ID, len(res.Chapters))
		}
		// skip_pdf：本来就不该有 PDF，属于正常出口。

	case len(res.PDFPaths) == 1:
		// 只有一章：单章 PDF 就是最终交付物，不需要再合并一次
		// （pdfcpu 合并单个文件只会白白多读多写一遍）。
		res.PDFPath = res.PDFPaths[0]

	case req.MergeChapters:
		out := req.OutputPDF
		if out == "" {
			out = filepath.Join(cfg.OutputDir, sanitizeOr(albumName, itoa(albumID))+".pdf")
		}
		prog.report(Progress{
			Stage:   "merge",
			Total:   len(res.PDFPaths),
			Message: "正在合并 " + itoa(len(res.PDFPaths)) + " 个章节 PDF",
		})
		if err := utils.MergePDFs(res.PDFPaths, out); err != nil {
			return res, protocol.Wrap(protocol.CodeIO, err, "合并章节 PDF 失败")
		}
		res.PDFPath = out
	}

	// ---- 加密 ----
	if cfg.PDFPassword != "" && res.PDFPath != "" {
		prog.report(Progress{Stage: "encrypt", Message: "正在加密 PDF"})
		enc, err := encryptInPlace(res.PDFPath, cfg.PDFPassword, cfg.PDFOwnerPassword)
		if err != nil {
			return res, err
		}
		res.PDFPath = enc
		res.Encrypted = true
	}

	// ---- 清理 ----
	//
	// 原始混淆图只在「用户没要求保留」且「没开断点续传」时才删：
	// skip_existing=true 说明用户希望重跑时复用已下好的文件，
	// 把原始图删掉等于让续传能力失效（下次还得重新下载）。
	// 没删的时候保留 RawDir 上报，让用户知道文件还在哪。
	if req.SkipImages {
		removeRestoredImages(res.Chapters)
		res.Chapters = clearImageDirs(res.Chapters)
	}
	if !req.KeepRaw && !cfg.SkipExisting {
		removeRawImages(res.Chapters)
		res.Chapters = clearRawDirs(res.Chapters)
	}

	res.ElapsedMS = time.Since(start).Milliseconds()
	prog.report(Progress{
		Stage:   "done",
		Done:    len(targets),
		Total:   len(targets),
		Message: "完成",
		Extra:   map[string]any{"pdf_path": res.PDFPath, "failed": res.FailedTotal},
	})
	return res, nil
}
