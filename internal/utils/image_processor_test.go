package utils

import (
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"testing"

	"github.com/disintegration/imaging"
)

// jsScrambleImage 是 APP 端 Function.js「还原被切的圖」那个 canvas 循环的
// **逐行直译**，只用于测试。
//
// 原文（onImageLoaded）：
//
//	var remainder = parseInt(h % num);
//	for (var i = 0; i < num; i++) {
//	  var copyH = Math.floor(h / num);
//	  var py = copyH * i;
//	  var y = h - copyH * (i + 1) - remainder;
//	  if (i == 0) { copyH = copyH + remainder; } else { py = py + remainder; }
//	  ctx.drawImage(img, 0, y, copyW, copyH, 0, py, copyW, copyH);
//	}
//
// 它和被测的 restorePixels 是两份独立实现：后者用单次 draw.Draw 做行块拷贝，
// 前者逐段贴图。两份实现必须像素级一致 —— 这就是本文件的核心理由。
func jsScrambleImage(src image.Image, num int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	remainder := h % num

	for i := 0; i < num; i++ {
		copyH := h / num // Math.floor(h / num)
		py := copyH * i
		y := h - copyH*(i+1) - remainder

		if i == 0 {
			copyH = copyH + remainder
		} else {
			py = py + remainder
		}
		if copyH <= 0 {
			continue
		}

		// ctx.drawImage(img, 0, y, copyW, copyH, 0, py, copyW, copyH)
		draw.Draw(dst,
			image.Rect(0, py, w, py+copyH),
			src,
			image.Pt(0, y),
			draw.Src)
	}
	return dst
}

// scrambleImage 是还原的逆变换：把原图弄成「还原后就是它自己」的样子。
//
// 推导：还原操作把【输入图】的第 i 个源区间
//
//	srcY_i     = h - segH*(i+1) - rem
//	height_i   = segH + (i==0 ? rem : 0)
//
// 搬到输出的
//
//	dstY_i     = segH*i + (i==0 ? 0 : rem)
//
// 位置。要让 restore(S) == O，只需令 S 的该源区间里装着 O 的对应区间。
func scrambleImage(orig image.Image, num int) image.Image {
	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	segH := h / num
	rem := h % num

	out := image.NewRGBA(image.Rect(0, 0, w, h))

	for i := 0; i < num; i++ {
		height := segH
		dstY := segH*i + rem
		if i == 0 {
			height = segH + rem
			dstY = 0
		}
		srcY := h - segH*(i+1) - rem
		if height <= 0 {
			continue
		}
		draw.Draw(out,
			image.Rect(0, srcY, w, srcY+height),
			orig,
			image.Pt(0, dstY),
			draw.Src)
	}
	return out
}

// bandedImage 生成一张「每行颜色都不同」的测试图。
//
// 用逐行渐变而不是纯色块：这样任何一行发生位移都会立刻被发现，
// 纯色块图像在整块错位时可能碰巧"看起来一样"。
func bandedImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := color.RGBA{
			R: uint8(y % 251),
			G: uint8((y * 7) % 253),
			B: uint8((y * 13) % 255),
			A: 255,
		}
		for x := 0; x < w; x++ {
			// 横向也加点变化，避免"整行拷贝 vs 整列拷贝"这类错误被掩盖
			c2 := c
			c2.R = uint8((int(c.R) + x) % 256)
			img.SetRGBA(x, y, c2)
		}
	}
	return img
}

// TestRestorePixelsMatchesJS 用「逐行直译的 canvas 循环」对拍 restorePixels。
//
// 覆盖多个段数、多个图片高度（刻意包含除不尽、余数很大的情况）。
func TestRestorePixelsMatchesJS(t *testing.T) {
	widths := []int{1, 7}
	heights := []int{101, 256, 480, 1001}
	segments := []int{2, 4, 6, 8, 10, 12, 14, 16, 18, 20}

	for _, w := range widths {
		for _, h := range heights {
			src := bandedImage(w, h)
			for _, num := range segments {
				if num > h {
					continue // 段数超过像素高度时原算法本身没有意义
				}

				got := restorePixels(src, num)
				want := jsScrambleImage(src, num)

				if !imagesEqual(got, want) {
					t.Fatalf("restorePixels 与 JS 直译不一致: w=%d h=%d segments=%d", w, h, num)
				}
			}
		}
	}
}

// TestRestoreRoundTrip 验证「打乱 → 还原 → 得到原图」这条闭环。
//
// 这是真正意义上的正确性测试：不依赖任何参照实现，
// 只断言还原确实是打乱的逆运算。
func TestRestoreRoundTrip(t *testing.T) {
	const w = 13
	heights := []int{100, 101, 233, 480, 999}
	segments := []int{2, 4, 10, 18, 20}

	for _, h := range heights {
		orig := bandedImage(w, h)
		for _, num := range segments {
			if num > h {
				continue
			}

			scrambled := scrambleImage(orig, num)
			// 自检：打乱后的图必须与原图不同，否则这个用例证明不了什么。
			if imagesEqual(scrambled, orig) {
				t.Fatalf("打乱结果与原图相同（h=%d num=%d），测试用例无意义", h, num)
			}

			restored := restorePixels(scrambled, num)
			if !imagesEqual(restored, orig) {
				t.Fatalf("还原后与原图不一致: h=%d segments=%d (余数=%d)", h, num, h%num)
			}
		}
	}
}

// TestDecodeImageFilePassthrough 确认段数 <=1 时原样转存。
func TestDecodeImageFilePassthrough(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "00001.png")
	dst := filepath.Join(dir, "out.png")

	orig := bandedImage(9, 40)
	if err := imaging.Save(orig, src); err != nil {
		t.Fatalf("准备测试图失败: %v", err)
	}

	// aid 远小于 scramble_id → 段数 0 → 原样转存
	if err := DecodeImageFileWithSegments(DecodeAndSaveTask{
		ImgSrcPath:      src,
		DecodedSavePath: dst,
		Name:            "00001",
	}, 0); err != nil {
		t.Fatalf("还原失败: %v", err)
	}

	got, err := imaging.Open(dst)
	if err != nil {
		t.Fatalf("读回结果失败: %v", err)
	}
	if !imagesEqual(got, orig) {
		t.Errorf("段数 0 时应当原样转存，实际内容变了")
	}
}

// TestDecodeImageFileRestoresBytes 走一遍完整文件链路：打乱图 → 还原图文件。
//
// 用 PNG 而非 JPEG 落盘：JPEG 有损，逐像素比对必然不等（见下面的 JPEG 用例）。
func TestDecodeImageFileRestoresBytes(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "00011.png")
	dstPath := filepath.Join(dir, "00011.png")

	const num = 10
	orig := bandedImage(11, 205) // 205 % 10 = 5，余数非零
	scrambled := scrambleImage(orig, num)

	if err := imaging.Save(scrambled, srcPath); err != nil {
		t.Fatalf("准备测试图失败: %v", err)
	}

	if err := DecodeImageFileWithSegments(DecodeAndSaveTask{
		ImgSrcPath:      srcPath,
		DecodedSavePath: dstPath,
		Name:            "00011",
	}, num); err != nil {
		t.Fatalf("还原失败: %v", err)
	}

	got, err := imaging.Open(dstPath)
	if err != nil {
		t.Fatalf("读回结果失败: %v", err)
	}
	if !imagesEqual(got, orig) {
		t.Errorf("落盘后的结果与原图不一致")
	}
	if _, err := os.Stat(dstPath); err != nil {
		t.Errorf("输出文件不存在: %v", err)
	}
}

// TestDecodeImageFileJPEGOutput 确认还原默认输出的 JPEG 是「正确的图」而非无损图。
//
// 产品约束是还原阶段一律出 JPEG（PDF 链路不支持 webp），所以必须确认
// 这条路径真的能走通、且像素重排确实生效（不是原图照抄）。
//
// 用平滑图像而非逐像素噪声图：JPEG 对后者是有损重灾区（MAE 能到 8 以上），
// 那测出来的是 JPEG 本身的性质，不是还原的正确性。真实漫画页是线条 + 平涂，
// 更接近 smoothImage 的形态。
func TestDecodeImageFileJPEGOutput(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "00011.png")
	dstPath := filepath.Join(dir, "00011.jpg")

	const num = 10
	orig := smoothImage(16, 205)
	scrambled := scrambleImage(orig, num)

	if err := imaging.Save(scrambled, srcPath); err != nil {
		t.Fatalf("准备测试图失败: %v", err)
	}
	if err := DecodeImageFileWithSegments(DecodeAndSaveTask{
		ImgSrcPath:      srcPath,
		DecodedSavePath: dstPath,
		Name:            "00011",
	}, num); err != nil {
		t.Fatalf("还原失败: %v", err)
	}

	got, err := imaging.Open(dstPath)
	if err != nil {
		t.Fatalf("读回 JPEG 失败: %v", err)
	}

	if got.Bounds().Dx() != orig.Bounds().Dx() || got.Bounds().Dy() != orig.Bounds().Dy() {
		t.Fatalf("尺寸变了: %v, 期望 %v", got.Bounds(), orig.Bounds())
	}

	diffOrig := meanAbsDiff(got, orig)
	diffScrambled := meanAbsDiff(got, scrambled)

	// 主断言：结果必须更接近「还原目标」而不是「打乱图」。
	// 这条能同时抓住「方向反了」和「根本没重排」两类问题。
	if diffScrambled < diffOrig {
		t.Errorf("结果更像打乱图（MAE 打乱=%.2f < 原图=%.2f），说明还原方向反了",
			diffScrambled, diffOrig)
	}
	// 次断言：JPEG 损失必须在合理范围内。
	if diffOrig > 5.0 {
		t.Errorf("与还原目标差距过大（MAE=%.2f），像素重排可能没生效", diffOrig)
	}
}

// smoothImage 生成一张「大色带 + 缓慢渐变」的平滑灰度图。
//
// 形态接近真实漫画页（线条 + 平涂），专门用于评估有损编码的影响。
func smoothImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		base := (y / 16 * 37) % 256
		for x := 0; x < w; x++ {
			v := uint8((base + x/4) % 256)
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// meanAbsDiff 计算两张同尺寸图的平均绝对像素误差（0..255 量级）。
func meanAbsDiff(a, b image.Image) float64 {
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return 1e9
	}
	var sum float64
	var n float64
	for y := ab.Min.Y; y < ab.Max.Y; y++ {
		for x := ab.Min.X; x < ab.Max.X; x++ {
			r1, g1, b1, _ := a.At(x, y).RGBA()
			r2, g2, b2, _ := b.At(x, y).RGBA()
			sum += abs(float64(r1>>8)-float64(r2>>8)) +
				abs(float64(g1>>8)-float64(g2>>8)) +
				abs(float64(b1>>8)-float64(b2>>8))
			n += 3
		}
	}
	if n == 0 {
		return 0
	}
	return sum / n
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// TestDecodeImageFileRejectsWebpOutput 确认拒绝写出 webp。
//
// 这是产品约束的守门人：PDF 链路不支持 webp，还原阶段必须落成 jpg/png。
func TestDecodeImageFileRejectsWebpOutput(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "00001.png")
	if err := imaging.Save(bandedImage(4, 40), srcPath); err != nil {
		t.Fatalf("准备测试图失败: %v", err)
	}

	err := DecodeImageFile(DecodeAndSaveTask{
		ImgSrcPath:      srcPath,
		DecodedSavePath: filepath.Join(dir, "out.webp"),
	})
	if err == nil {
		t.Fatalf("写出 .webp 应当被拒绝（imaging 写不出 webp，且 PDF 链路不支持）")
	}
}

// TestDecodeImageFileMissingSource 确认源文件缺失时给出错误而非 panic。
func TestDecodeImageFileMissingSource(t *testing.T) {
	err := DecodeImageFile(DecodeAndSaveTask{
		ImgSrcPath:      filepath.Join(t.TempDir(), "不存在.png"),
		DecodedSavePath: filepath.Join(t.TempDir(), "out.png"),
	})
	if err == nil {
		t.Fatalf("源文件不存在时应当报错")
	}
}

// TestBatchDecodeAndSaveOrder 确认批量还原的结果与入参一一对应（不按 URL 查找）。
func TestBatchDecodeAndSaveOrder(t *testing.T) {
	dir := t.TempDir()

	// 故意让所有任务同名（模拟"同一批里出现重复文件名"），
	// 结果顺序错位的实现会在这里暴露。
	var items []DecodeAndSaveTask
	for i := 0; i < 4; i++ {
		p := filepath.Join(dir, "src", "0000"+string(rune('1'+i))+".png")
		if err := EnsureDir(filepath.Dir(p)); err != nil {
			t.Fatal(err)
		}
		if err := imaging.Save(bandedImage(5, 50), p); err != nil {
			t.Fatal(err)
		}
		items = append(items, DecodeAndSaveTask{
			ImgSrcPath:      p,
			DecodedSavePath: filepath.Join(dir, "out", "0000"+string(rune('1'+i))+".jpg"),
			Name:            "00001", // 全部同名，但 aid 相同时段数相同，仍然应当各自成功
		})
	}

	var progress []int
	results := BatchDecodeAndSaveWithProgress(DefaultScrambleID, 399054, items, 3,
		func(done, total int, _ DecodeAndSaveResult) {
			progress = append(progress, done)
		})

	if len(results) != len(items) {
		t.Fatalf("结果数量 %d != 任务数量 %d", len(results), len(items))
	}
	for i, r := range results {
		if !r.OK() {
			t.Errorf("第 %d 个任务失败: %v", i, r.Err)
		}
		if r.ImgSrcPath != items[i].ImgSrcPath {
			t.Errorf("第 %d 个结果与入参不对应: %q != %q", i, r.ImgSrcPath, items[i].ImgSrcPath)
		}
		if r.DecodedSavePath != items[i].DecodedSavePath {
			t.Errorf("第 %d 个输出路径不对应: %q != %q", i, r.DecodedSavePath, items[i].DecodedSavePath)
		}
	}

	// 进度回调次数必须等于任务数（每个任务恰好一次）
	if len(progress) != len(items) {
		t.Errorf("进度回调 %d 次，期望 %d 次", len(progress), len(items))
	}
}

// TestDecodeImageFileLandsAtomically 锁住「还原产物是 rename 落地的」这个属性。
//
// 为什么值得单独测：下游判断一页有没有还原只看「文件存在」（fileExists），
// 所以写到一半的半截 jpg 会被当成有效输入喂进 PDF —— 页数还是对的、内容却是坏的，
// 比缺页更难发现。这里断言产物落在目标路径上、且输出目录里**不留** .part：
// 最后那条能抓住"改成临时文件后忘了改名/忘了清理"的回归。
func TestDecodeImageFileLandsAtomically(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "00001.png")
	outDir := filepath.Join(dir, "out")
	dst := filepath.Join(outDir, "00001.jpg")

	if err := imaging.Save(bandedImage(9, 40), src); err != nil {
		t.Fatalf("准备测试图失败: %v", err)
	}

	// 段数 0 → 原样转存，走的仍是同一条落盘路径。
	if err := DecodeImageFileWithSegments(DecodeAndSaveTask{
		ImgSrcPath:      src,
		DecodedSavePath: dst,
		Name:            "00001",
	}, 0); err != nil {
		t.Fatalf("还原失败: %v", err)
	}

	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("成品没有落到目标路径 %q: %v", dst, err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("读取输出目录失败: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		if filepath.Ext(e.Name()) == ".part" {
			t.Errorf("输出目录残留了中间文件: %s（应当是 rename 后不留痕）", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("输出目录应当只有 1 个成品，实际 %d 个: %v", len(entries), names)
	}
}

// ---- 辅助 ----

// imagesEqual 逐像素比较两张图（尺寸或任一像素不同即不等）。
func imagesEqual(a, b image.Image) bool {
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return false
	}
	for y := ab.Min.Y; y < ab.Max.Y; y++ {
		for x := ab.Min.X; x < ab.Max.X; x++ {
			r1, g1, b1, a1 := a.At(x, y).RGBA()
			r2, g2, b2, a2 := b.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				return false
			}
		}
	}
	return true
}
