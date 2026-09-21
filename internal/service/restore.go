package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// RestoreImagesRequest 是「还原本地已有的混淆图片」的请求。
//
// 用途：图片已经在本地（上次下载过、或从别处拿到的），只需要还原，
// 不想重新走一遍下载。整本下载流程内部用的也是同一套还原逻辑。
type RestoreImagesRequest struct {
	// AID 参与段数计算的 aid。
	//
	// 注意它应该是**阅读页返回的 id**，不是作品号 —— 与 APP 端
	// get_num(readList.id, img.alt) 保持一致，传错会还原出花屏。
	AID int `json:"aid"`
	// ScrambleID 切图阈值；<=0 时用配置值 / 默认值。
	ScrambleID int `json:"scramble_id"`

	// InputDir 原始（混淆）图片所在目录。
	InputDir string `json:"input_dir"`
	// Images 显式图片列表；给出时忽略 InputDir。
	Images []string `json:"images"`
	// OutputDir 还原结果输出目录。
	OutputDir string `json:"output_dir"`
	// Format 输出格式："jpg"（默认）或 "png"。
	Format string `json:"format"`
	// Workers 并发数；<=0 用配置值。
	Workers int `json:"workers"`
	// SkipExisting 跳过已存在且非空的输出文件。
	SkipExisting bool `json:"skip_existing"`
}

// RestoreResult 是还原结果。
type RestoreResult struct {
	Total     int    `json:"total"`
	Restored  int    `json:"restored"`
	Failed    int    `json:"failed"`
	OutputDir string `json:"output_dir"`
	// Failures 最多回传前 maxReportedFailures 条。
	Failures []ImageFailure `json:"failures,omitempty"`
	// DroppedFailures 因超过上限未列出的失败条数。
	DroppedFailures int `json:"dropped_failures,omitempty"`
}

// RestoreImages 批量还原本地图片。
func (s *Service) RestoreImages(ctx context.Context, prog ProgressFunc, req RestoreImagesRequest) (*RestoreResult, error) {
	cfg := s.Config()

	scrambleID := req.ScrambleID
	if scrambleID <= 0 {
		scrambleID = cfg.ScrambleID
	}
	if req.AID <= 0 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"aid 必须 > 0：段数计算依赖它（用阅读页返回的 id，不是作品号）")
	}

	paths, outDir, err := s.resolveRestoreInputs(cfg, req)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "在 %s 下没有找到任何图片", req.InputDir)
	}

	ext := normalizeRestoreExt(req.Format)
	workers := req.Workers
	if workers <= 0 {
		workers = cfg.EffectiveWorkers()
	}

	res := &RestoreResult{Total: len(paths), OutputDir: outDir}

	var (
		tasks    []utils.DecodeAndSaveTask
		gifTasks []utils.DecodeAndSaveTask
		skipped  int
	)

	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))

		dstExt := ext
		isGIF := strings.EqualFold(filepath.Ext(p), ".gif")
		if isGIF {
			dstExt = ".gif"
		}
		dst := filepath.Join(outDir, name+dstExt)

		if req.SkipExisting && fileExists(dst) {
			skipped++
			continue
		}

		task := utils.DecodeAndSaveTask{
			ImgSrcPath:      p,
			DecodedSavePath: dst,
			Name:            name,
		}
		if isGIF {
			// GIF 不参与切图还原（与 APP 端 scramble_image 的早退分支一致），
			// 原样转存即可 —— 丢进批量还原会被按 aid 算出的段数切开，反而毁图。
			gifTasks = append(gifTasks, task)
			continue
		}
		tasks = append(tasks, task)
	}

	// GIF：直接复制，不计入还原进度（没有真正的还原工作）。
	for _, t := range gifTasks {
		if err := copyFile(t.ImgSrcPath, t.DecodedSavePath); err != nil {
			if len(res.Failures) >= maxReportedFailures {
				res.DroppedFailures++
			} else {
				res.Failures = append(res.Failures, ImageFailure{
					URL: t.ImgSrcPath, Stage: "restore",
					Error: "GIF 转存失败: " + msgOf(err),
				})
			}
			continue
		}
		res.Restored++
	}

	if len(tasks) > 0 {
		results := utils.BatchDecodeAndSaveWithProgress(
			scrambleID, req.AID, tasks, workers,
			func(done, total int, r utils.DecodeAndSaveResult) {
				prog.report(Progress{
					Stage:   "restore",
					Done:    done,
					Total:   total,
					Message: "还原 " + filepath.Base(r.ImgSrcPath),
					Extra:   map[string]any{"segments": r.Segments},
				})
			})

		for i := range results {
			if results[i].OK() {
				res.Restored++
				continue
			}
			if len(res.Failures) >= maxReportedFailures {
				res.DroppedFailures++
				continue
			}
			res.Failures = append(res.Failures, ImageFailure{
				URL:   results[i].ImgSrcPath,
				Stage: "restore",
				Error: msgOf(results[i].Err),
			})
		}
	}

	res.Restored += skipped
	res.Failed = len(res.Failures) + res.DroppedFailures
	return res, nil
}

// resolveRestoreInputs 决定还原的输入文件列表与输出目录。
func (s *Service) resolveRestoreInputs(cfg *config.Config, req RestoreImagesRequest) ([]string, string, error) {
	outDir := strings.TrimSpace(req.OutputDir)
	if outDir == "" {
		outDir = cfg.OutputDir
	}

	if len(req.Images) > 0 {
		paths := make([]string, 0, len(req.Images))
		for _, p := range req.Images {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		return paths, outDir, nil
	}

	dir := strings.TrimSpace(req.InputDir)
	if dir == "" {
		return nil, outDir, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, outDir, protocol.Wrap(protocol.CodeIO, err, "读取目录 %s 失败", dir)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if utils.IsImageFile(e.Name()) {
			names = append(names, e.Name())
		}
	}
	utils.SortImageNames(names)

	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, filepath.Join(dir, n))
	}
	return paths, outDir, nil
}
