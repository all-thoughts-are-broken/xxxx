package utils

import (
	"fmt"
	"image"
	"image/draw"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/disintegration/imaging"
)

// DecodeAndSaveTask 描述一次「还原单张被切分图片」的任务。
type DecodeAndSaveTask struct {
	// AID 作品号，参与段数计算。
	AID int
	// ScrambleID 切图阈值，取自阅读页响应的 scramble_id；为 0 时用 DefaultScrambleID。
	ScrambleID int
	// ImgSrcPath 混淆图（服务端原始返回）的本地路径。
	ImgSrcPath string
	// DecodedSavePath 还原结果的保存路径。
	//
	// 推荐 .jpg：PDF 生成链路不支持 webp，统一落成 JPEG 可以直接喂给 PDF 库，
	// 也避免同一批图出现多种格式。扩展名决定输出格式，webp 只能读不能写。
	DecodedSavePath string
	// Name 参与段数计算的图片名（不含扩展名，例如 "00011"）。
	// 留空时自动取 ImgSrcPath 的文件名去扩展名。
	Name string
}

// DecodeAndSaveResult 是单张图的还原结果。
type DecodeAndSaveResult struct {
	ImgSrcPath      string
	DecodedSavePath string
	// Segments 实际切分的段数；0 表示该图未被切分，原样转存。
	Segments int
	// Err 为 nil 表示成功。
	Err error
}

// OK 报告该任务是否成功。
func (r DecodeAndSaveResult) OK() bool { return r.Err == nil }

// encodableExts 是 imaging 能写出的格式集合（注意不含 webp）。
var encodableExts = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".bmp": {}, ".tif": {}, ".tiff": {},
}

// DecodeAndSave 还原单张被切分的图片。
//
// 保留原签名以便平滑迁移，内部转成基于 task 的实现。
func DecodeAndSave(scrambleId, aid int, imgSrcPath, decodedSavePath string) error {
	return DecodeImageFile(DecodeAndSaveTask{
		AID:             aid,
		ScrambleID:      scrambleId,
		ImgSrcPath:      imgSrcPath,
		DecodedSavePath: decodedSavePath,
	})
}

// DecodeImageFile 还原单张被切分的图片并写出。
//
// 还原规则与 APP 端 scramble_image 完全一致：把整张图按 segments 段纵向等分，
// 段与段顺序整体颠倒；余数像素并入「原图最下方那一段」（即还原后位于最上方的一段）。
// segments <= 1 时图片未被切分，原样转存。
func DecodeImageFile(task DecodeAndSaveTask) error {
	segments := CountImageSegments(task.ScrambleID, task.AID, task.imageName())
	return decodeWithSegments(task, segments)
}

// DecodeImageFileWithSegments 用显式给定的段数还原。
//
// 服务层在批量场景下会先把段数算好（段数只依赖 aid + 文件名，与图片内容无关），
// 一批准 40 张图没必要重复做 40 次 MD5。
func DecodeImageFileWithSegments(task DecodeAndSaveTask, segments int) error {
	return decodeWithSegments(task, segments)
}

func (t DecodeAndSaveTask) imageName() string {
	if t.Name != "" {
		return t.Name
	}
	return filepath.Base(t.ImgSrcPath)
}

func decodeWithSegments(task DecodeAndSaveTask, segments int) error {
	if task.ImgSrcPath == "" {
		return fmt.Errorf("还原图片: 源路径为空")
	}
	if task.DecodedSavePath == "" {
		return fmt.Errorf("还原图片: 目标路径为空")
	}

	ext := strings.ToLower(filepath.Ext(task.DecodedSavePath))
	if _, ok := encodableExts[ext]; !ok {
		return fmt.Errorf("还原图片: 不支持写出 %q 格式（webp 只能读不能写），请把输出改为 .jpg 或 .png", ext)
	}

	src, err := imaging.Open(task.ImgSrcPath)
	if err != nil {
		return fmt.Errorf("还原图片: 打开 %q 失败: %w", task.ImgSrcPath, err)
	}

	bounds := src.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return fmt.Errorf("还原图片: %q 尺寸异常 %dx%d", task.ImgSrcPath, bounds.Dx(), bounds.Dy())
	}

	out := src
	if segments > 1 {
		out = restorePixels(src, segments)
	}

	if err := EnsureDir(filepath.Dir(task.DecodedSavePath)); err != nil {
		return err
	}

	// 先写临时文件再改名，和下载链路保持同一套纪律。
	//
	// 直接写成品的话，写到一半失败（磁盘满、进程被杀）会留下一张**能被打开但内容
	// 不全**的 jpg；而下游只看「文件存在」就当作已还原，于是半截图被静默喂进 PDF。
	//
	// 临时名用 `<成品>.part`（与下载链路的约定一致）而不是「保留原扩展名」：
	// 下游是按扩展名白名单扫目录取图的，`.part-00001.jpg` 会被当成正常图片收走。
	// 代价是 imaging.Save 推断不出格式，得显式 Encode —— 与 Save 内部做的事一致。
	tmp := task.DecodedSavePath + ".part"
	format, err := imaging.FormatFromFilename(task.DecodedSavePath)
	if err != nil {
		return fmt.Errorf("还原图片: 无法从 %q 推断输出格式: %w", task.DecodedSavePath, err)
	}
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("还原图片: 创建 %q 失败: %w", tmp, err)
	}
	if err := imaging.Encode(f, out, format); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("还原图片: 写出 %q 失败: %w", task.DecodedSavePath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("还原图片: 关闭 %q 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, task.DecodedSavePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("还原图片: 落盘 %q 失败: %w", task.DecodedSavePath, err)
	}
	return nil
}

// restorePixels 执行真正的像素重排。
//
// 算法与原实现一致，但把逐段的 imaging.Paste 换成单次 draw.Draw。
// imaging.Paste 每贴一段都会把整张画布完整拷贝一遍，段数 20 时就是 20 张全图拷贝；
// 这里只做 segments 次行块拷贝，量级差一个维度。
func restorePixels(src image.Image, segments int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	segmentHeight := height / segments
	remainder := height % segments

	dst := image.NewNRGBA(image.Rect(0, 0, width, height))

	dstY := 0
	for i := 0; i < segments; i++ {
		currentSegmentHeight := segmentHeight
		if i == 0 {
			// 余数像素并入原图最下方那一段（还原后位于最上方）
			currentSegmentHeight += remainder
		}

		srcY := height - (segmentHeight*(i+1) + remainder)
		if srcY < 0 {
			srcY = 0
		}
		if srcY+currentSegmentHeight > height {
			currentSegmentHeight = height - srcY
		}
		if currentSegmentHeight <= 0 {
			continue
		}

		srcRect := image.Rect(
			bounds.Min.X, bounds.Min.Y+srcY,
			bounds.Min.X+width, bounds.Min.Y+srcY+currentSegmentHeight,
		)
		dstRect := image.Rect(0, dstY, width, dstY+currentSegmentHeight)
		draw.Draw(dst, dstRect, src, srcRect.Min, draw.Src)

		dstY += currentSegmentHeight
	}
	return dst
}

// BatchDecodeAndSave 并发还原一批图片，返回与入参等长的结果切片（顺序一一对应）。
//
// 保留原签名；需要感知进度时用 BatchDecodeAndSaveWithProgress。
func BatchDecodeAndSave(scrambleId, aid int, items []DecodeAndSaveTask, workers int) []DecodeAndSaveResult {
	return BatchDecodeAndSaveWithProgress(scrambleId, aid, items, workers, nil)
}

// BatchDecodeAndSaveWithProgress 与 BatchDecodeAndSave 相同，但每完成一张回调一次。
//
// 说明：scrambleId / aid 会覆盖每个 task 自带的同名字段，方便调用方批量传参。
// onDone 在 worker 内串行调用，实现里不要做重活。done 的值是已完成数量（1-based）。
func BatchDecodeAndSaveWithProgress(
	scrambleId, aid int,
	items []DecodeAndSaveTask,
	workers int,
	onDone func(done, total int, res DecodeAndSaveResult),
) []DecodeAndSaveResult {
	results := make([]DecodeAndSaveResult, len(items))
	if len(items) == 0 {
		return results
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	// 段数只依赖 (scrambleId, aid, name)，与文件内容无关：主线程先算好，
	// 避免每个 worker 重复做 MD5。
	segments := make([]int, len(items))
	for i := range items {
		items[i].AID = aid
		items[i].ScrambleID = scrambleId
		segments[i] = CountImageSegments(scrambleId, aid, items[i].imageName())
	}

	var (
		mu      sync.Mutex
		done    int
		nextIdx int
	)
	total := len(items)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if nextIdx >= total {
					mu.Unlock()
					return
				}
				i := nextIdx
				nextIdx++
				mu.Unlock()

				it := items[i]
				res := DecodeAndSaveResult{
					ImgSrcPath:      it.ImgSrcPath,
					DecodedSavePath: it.DecodedSavePath,
					Segments:        segments[i],
				}
				res.Err = decodeWithSegments(it, segments[i])
				results[i] = res

				mu.Lock()
				done++
				if onDone != nil {
					onDone(done, total, res)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return results
}

// EnsureDir 确保目录存在；dir 为空或 "." 时直接返回。
func EnsureDir(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %q 失败: %w", dir, err)
	}
	return nil
}
