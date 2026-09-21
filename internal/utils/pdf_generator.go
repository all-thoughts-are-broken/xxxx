package utils

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/disintegration/imaging"
	"github.com/jung-kurt/gofpdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// DefaultPDFMaxPageHeight 是单个 PDF 页面高度的默认上限（点）；0 表示不限制。
//
// 当前产品形态是「一章 = 一个只有一页的长 PDF」，全部内容塞进一条长页，
// 所以默认不限制、不做任何缩放。
//
// 提示：PDF 规范本身不限页高，但部分阅读器（尤其 Acrobat）超过 200 英寸
// = 14400pt 会拒绝打开。若宿主遇到这种兼容性问题，可通过 pdf_max_page_height
// 打开上限，届时整页会等比缩放——只作用于页面的 CTM，图片仍以原始分辨率
// 嵌入，不损失画质。
const DefaultPDFMaxPageHeight = 0.0

// PageImage 是参与 PDF 合成的一张图。
type PageImage struct {
	width    float64
	height   float64
	path     string
	bookmark string

	// gofpdf 原生支持的格式（jpg/png/gif）直接按路径注册；
	// 其它格式（webp/bmp/tiff/avif）先解码重编码到内存，再用 Reader 注册。
	// 为空表示走路径注册。
	encoded   []byte
	imageType string
}

// PDFOptions 控制 PDF 合成行为。
type PDFOptions struct {
	// Chapter 顶层书签（章节名）；为空则不建顶层书签。
	Chapter string
	// MaxPageHeight 单页最大高度（pt）。<=0 时用 DefaultPDFMaxPageHeight。
	MaxPageHeight float64
	// Layout "single"（整本一条长页，默认）或 "paged"（按 MaxPageHeight 分页）。
	Layout string
	// PerImageBookmark 是否为每张图建二级书签。
	PerImageBookmark bool
}

func (o PDFOptions) maxPageHeight() float64 {
	if o.MaxPageHeight <= 0 {
		return DefaultPDFMaxPageHeight
	}
	return o.MaxPageHeight
}

func (o PDFOptions) layout() string {
	if o.Layout == "paged" {
		return "paged"
	}
	return "single"
}

// mostFrequent 返回数组中出现频率最高的元素。数组为空时 ok 为 false。
func mostFrequent(arr []float64) (value float64, ok bool) {
	if len(arr) == 0 {
		return 0, false
	}

	frequency := make(map[float64]int)
	maxCount := 0

	// 按输入顺序遍历（而不是遍历 map），保证同频时取先出现的那个，结果可复现。
	for _, num := range arr {
		frequency[num]++
		if frequency[num] > maxCount {
			maxCount = frequency[num]
			value = num
			ok = true
		}
	}
	return value, ok
}

// adjustImagesToWidth 把所有图片宽度缩放到 targetWidth，高度按比例缩放。
// 宽度为 0（尺寸测量失败）的条目保持原样。
func adjustImagesToWidth(imageInfos []PageImage, targetWidth float64) []PageImage {
	adjusted := make([]PageImage, len(imageInfos))
	for i, img := range imageInfos {
		adjusted[i] = img
		if img.width <= 0 || img.width == targetWidth {
			continue
		}
		scale := targetWidth / img.width
		adjusted[i].width = targetWidth
		adjusted[i].height = img.height * scale
	}
	return adjusted
}

// normalizeImageWidths 把一组图片统一到「出现次数最多的那个宽度」。
//
// 之所以取众数而不是第一张：作品里偶尔会混进一张尺寸不同的图（跨页、广告页），
// 取第一张的话整本都会被那一张带偏。
func normalizeImageWidths(images []PageImage) []PageImage {
	widths := make([]float64, 0, len(images))
	for _, img := range images {
		if img.width > 0 {
			widths = append(widths, img.width)
		}
	}
	targetWidth, ok := mostFrequent(widths)
	if !ok {
		return images
	}
	return adjustImagesToWidth(images, targetWidth)
}

// loadFittedPageImagesFromDir 读取目录下的所有图片并统一宽度。
//
// 只挑图片扩展名、跳过子目录；顺序按文件名做自然排序（2 排在 10 之前）。
func loadFittedPageImagesFromDir(dir string) ([]PageImage, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取图片目录 %q 失败: %w", dir, err)
	}

	names := make([]string, 0, len(dirEntries))
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		if !IsImageFile(de.Name()) {
			continue
		}
		names = append(names, de.Name())
	}
	SortImageNames(names)

	pageImages := make([]PageImage, 0, len(names))
	for _, name := range names {
		imagePath := filepath.Join(dir, name)

		// 只读文件头拿宽高：这里本来就不需要解码整张图，
		// 之前用 imaging.Open 等于把每张图完整解码一遍再丢给 gofpdf 重解码一次。
		cfg, err := DecodeConfigOf(imagePath)
		if err != nil {
			// 读不出来的图不能直接跳过，否则后面的页码会全部错位；插一张占位图。
			ph, phErr := placeholderPageImage(imagePath, name, cfg)
			if phErr != nil {
				return nil, fmt.Errorf("图片 %q 无法读取且占位图生成失败: %w", name, err)
			}
			pageImages = append(pageImages, ph)
			continue
		}

		pi, err := newPageImage(imagePath, cfg)
		if err != nil {
			ph, phErr := placeholderPageImage(imagePath, name, cfg)
			if phErr != nil {
				return nil, fmt.Errorf("图片 %q 处理失败且占位图生成失败: %w", name, err)
			}
			pageImages = append(pageImages, ph)
			continue
		}
		pageImages = append(pageImages, pi)
	}

	return normalizeImageWidths(pageImages), nil
}

// newPageImage 构造一个页面条目，并决定该图走「路径注册」还是「重编码注册」。
//
// gofpdf 只认 jpg/png/gif：这三种直接用路径注册，让 gofpdf 自己去读文件；
// 其它格式（webp/bmp/tiff/avif）先在内存里重编码成 jpg/png 再注册。
func newPageImage(path string, cfg image.Config) (PageImage, error) {
	pi := PageImage{
		width:    float64(cfg.Width),
		height:   float64(cfg.Height),
		path:     path,
		bookmark: bookmarkName(filepath.Base(path)),
	}

	if t := gofpdfImageType(path); t != "" {
		pi.imageType = t
		return pi, nil
	}

	data, encType, err := encodeImageForPDF(path)
	if err != nil {
		return PageImage{}, err
	}
	pi.encoded, pi.imageType = data, encType
	return pi, nil
}

// bookmarkName 由文件名推导页码书签文本：去掉扩展名并去掉前导 0（00012 → 12）。
//
// 规则沿用 pdf_test/tools.go 最后一次修改的版本：
//
//	bookmark := strings.TrimLeft(strings.TrimSuffix(base, filepath.Ext(base)), "0")
//
// 不要被 output/1472136.pdf 里的 "00001" 误导 —— 那批参考 PDF 是同一个文件
// 更早的版本生成的。书签是给人导航用的，去零更好读。
//
// 唯一偏离原版的地方：原名全是 0 时 TrimLeft 会得到空串，原版会写出一个
// 空标题书签（阅读器里表现为一个点不开的空节点），这里回退成原名。
func bookmarkName(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	out := strings.TrimLeft(base, "0")
	if out == "" {
		return base
	}
	return out
}

// outlineText 把书签标题编码成 PDF 大纲项（/Title）要求的形式：UTF-16BE + BOM。
//
// 为什么必须手动转：
//
// gofpdf 只在「当前字体是 UTF-8 字体」时才替我们转换（Bookmark 内部判 f.isCurrentUTF8）。
// 而本项目的 PDF 页面是纯图片、不加载任何字体，isCurrentUTF8 恒为 false ——
// gofpdf 于是把 Go 字符串**按原始字节**写进 /Title ( ... )。
// PDF 规范规定：不带 BOM 的字符串按 PDFDocEncoding 解释，于是 "第" 的 UTF-8 字节
// E7 AC AC 被读成 "ç¬¬"，在阅读器里整棵树都是乱码（实测 Acrobat/WPS 均如此）。
//
// 补上 FE FF BOM 后，解析器会按 UTF-16BE 解码，中文才能正确显示。
// 页码书签（纯数字）走这里也无害，只是每个字符多占一个字节。
//
// 与之配套的是 gofpdf 的 escape()：它只做字节级替换（\ ( ) \r），
// 不碰 >=0x80 的字节，所以二进制内容能安全穿过字面串。
func outlineText(s string) string {
	if s == "" {
		return s
	}
	buf := make([]byte, 0, 2+2*len(s))
	buf = append(buf, 0xFE, 0xFF) // UTF-16BE BOM
	for _, r := range s {
		if r > 0xFFFF {
			// 补充平面字符要拆成代理对，否则阅读器会显示成空白。
			hi, lo := utf16.EncodeRune(r)
			buf = append(buf, byte(hi>>8), byte(hi), byte(lo>>8), byte(lo))
			continue
		}
		buf = append(buf, byte(r>>8), byte(r))
	}
	return string(buf)
}

// GeneratePdfFromDir 把一个目录下的图片合成 PDF。
//
// 保留原签名。默认整本合成「一条长页」（适合长条漫画上下滚动阅读），
// 需要更强兼容性时用 GeneratePdfFromImages 配 PDFOptions{Layout: "paged"}。
func GeneratePdfFromDir(input, output, chapterName string) error {
	pageImages, err := loadFittedPageImagesFromDir(input)
	if err != nil {
		return fmt.Errorf("生成 PDF: %w", err)
	}
	return writeImagesToPDF(pageImages, output, PDFOptions{
		Chapter:          chapterName,
		PerImageBookmark: true,
	})
}

// GeneratePdfFromImages 按显式给定的图片列表合成 PDF（列表顺序即页面顺序）。
func GeneratePdfFromImages(images []string, output string, opts PDFOptions) error {
	if len(images) == 0 {
		return fmt.Errorf("生成 PDF: 图片列表为空")
	}
	pageImages, err := loadPageImages(images)
	if err != nil {
		return fmt.Errorf("生成 PDF: %w", err)
	}
	return writeImagesToPDF(pageImages, output, opts)
}

// loadPageImages 加载显式列表的图片并统一宽度。
//
// 容错边界分两档，和 loadFittedPageImagesFromDir 保持一致：
//
//   - 扩展名就不是图片 → 硬报错。这是调用方传错了参数，静默吞掉会让用户
//     以为整章都合进去了。
//   - 扩展名是图片但解码失败（文件截断 / 下载不完整 / 内容不是真图片）
//     → 换成占位图。跳过它会让后面每一页的页码前移，和真实章节错位，
//     而占位图上会写明是哪一张坏了，反而更容易定位。
func loadPageImages(paths []string) ([]PageImage, error) {
	pageImages := make([]PageImage, 0, len(paths))
	for _, p := range paths {
		if !IsImageFile(p) {
			return nil, fmt.Errorf("%q 不是支持的图片格式", p)
		}
		cfg, err := DecodeConfigOf(p)
		if err != nil {
			// 注意 cfg 此时是零值（DecodeConfigOf 出错时不返回部分结果），
			// 占位图会退回到 1000x1414 的 A4 比例。
			ph, phErr := placeholderPageImage(p, filepath.Base(p), cfg)
			if phErr != nil {
				return nil, fmt.Errorf("读取图片 %q 失败、占位图也生成失败: %w", p, err)
			}
			pageImages = append(pageImages, ph)
			continue
		}
		pi, err := newPageImage(p, cfg)
		if err != nil {
			ph, phErr := placeholderPageImage(p, filepath.Base(p), cfg)
			if phErr != nil {
				return nil, fmt.Errorf("处理图片 %q 失败、占位图也生成失败: %w", p, err)
			}
			pageImages = append(pageImages, ph)
			continue
		}
		pageImages = append(pageImages, pi)
	}
	return normalizeImageWidths(pageImages), nil
}

// writeImagesToPDF 是真正的合成实现。
func writeImagesToPDF(pageImages []PageImage, output string, opts PDFOptions) error {
	if len(pageImages) == 0 {
		return fmt.Errorf("生成 PDF: 没有可用的图片")
	}
	if err := EnsureDir(filepath.Dir(output)); err != nil {
		return err
	}

	pdf := gofpdf.NewCustom(&gofpdf.InitType{UnitStr: "pt"})
	pdf.SetMargins(0, 0, 0)
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetCompression(true)
	// 页面上不绘制文字（纯图片），因此不加载字体。
	// 原实现在这里 AddUTF8Font 了一个不存在的路径（./resources/fonts/...），
	// 会把 fpdf 的错误状态置位，导致随后的 pdf.Error() 必然非 nil —— PDF 永远生成失败。

	if opts.layout() == "paged" {
		drawPaged(pdf, pageImages, opts)
	} else {
		drawSingle(pdf, pageImages, opts)
	}

	if err := pdf.Error(); err != nil {
		return fmt.Errorf("生成 PDF: 绘制到 %q 失败: %w", output, err)
	}

	if err := pdf.OutputFileAndClose(output); err != nil {
		return fmt.Errorf("生成 PDF: 写出 %q 失败: %w", output, err)
	}
	return nil
}

// addPage 建新页并按需写顶层书签。
//
// 书签必须在这里写：gofpdf 的 Bookmark 记录的是「调用时刻的当前页」
// （内部取 f.PageNo()），所以不能先收集再统一写，否则所有书签都会被归到最后一页。
func addPage(pdf *gofpdf.Fpdf, w, h float64, opts PDFOptions) {
	pdf.AddPageFormat("", gofpdf.SizeType{Wd: w, Ht: h})
	if opts.Chapter != "" {
		pdf.Bookmark(outlineText(opts.Chapter), 0, 0)
	}
}

// addImageBookmark 写单张图的二级书签。没有章节时降级为一级，
// 避免构造出「没有父节点的 level 1」这种畸形大纲。
func addImageBookmark(pdf *gofpdf.Fpdf, img PageImage, y float64, opts PDFOptions) {
	if !opts.PerImageBookmark || img.bookmark == "" {
		return
	}
	level := 1
	if opts.Chapter == "" {
		level = 0
	}
	pdf.Bookmark(outlineText(img.bookmark), level, y)
}

// drawSingle 把整本画成一条长页；超出阅读器上限时整体等比缩放。
func drawSingle(pdf *gofpdf.Fpdf, images []PageImage, opts PDFOptions) {
	totalHeight := 0.0
	for _, img := range images {
		totalHeight += img.height
	}
	if totalHeight <= 0 {
		return
	}

	scale := 1.0
	if maxH := opts.maxPageHeight(); maxH > 0 && totalHeight > maxH {
		scale = maxH / totalHeight
	}
	addPage(pdf, images[0].width*scale, totalHeight*scale, opts)

	y := 0.0
	for _, img := range images {
		h := img.height * scale
		drawPageImage(pdf, img, 0, y, img.width*scale, h)
		addImageBookmark(pdf, img, y, opts)
		y += h
	}
}

// drawPaged 按 MaxPageHeight 分页，每页装完整的图；单张图自身超过上限时等比缩小。
func drawPaged(pdf *gofpdf.Fpdf, images []PageImage, opts PDFOptions) {
	maxH := opts.maxPageHeight()
	if maxH <= 0 {
		// 没设上限就等价于「整本一条长页」
		drawSingle(pdf, images, opts)
		return
	}
	pageWidth := maxWidthOf(images)

	i := 0
	for i < len(images) {
		// 组一页：尽量塞，但不切开任何一张图。
		pageHeight := 0.0
		j := i
		for j < len(images) {
			h := images[j].height
			if pageHeight+h > maxH && j > i {
				break
			}
			pageHeight += h
			j++
			if pageHeight >= maxH {
				break
			}
		}
		if pageHeight <= 0 {
			pageHeight = maxH
		}

		// 单张图就超过整页高度时，为它单独开一页并整体缩小。
		scale := 1.0
		if pageHeight > maxH {
			scale = maxH / pageHeight
			pageHeight = maxH
		}

		addPage(pdf, pageWidth*scale, pageHeight, opts)

		y := 0.0
		for k := i; k < j; k++ {
			img := images[k]
			h := img.height * scale
			drawPageImage(pdf, img, 0, y, img.width*scale, h)
			addImageBookmark(pdf, img, y, opts)
			y += h
		}
		i = j
	}
}

// maxWidthOf 取图片组里的最大宽度，作为分页模式下所有页面的统一页宽。
func maxWidthOf(images []PageImage) float64 {
	w := 0.0
	for _, img := range images {
		if img.width > w {
			w = img.width
		}
	}
	if w <= 0 {
		w = 1000
	}
	return w
}

// drawPageImage 把一张图放到 PDF 当前页上。
func drawPageImage(pdf *gofpdf.Fpdf, img PageImage, x, y, w, h float64) {
	// 用路径作为注册名：同一次合成里同名只会注册一次，gofpdf 自动去重。
	name := img.path
	opts := gofpdf.ImageOptions{ImageType: img.imageType}

	if len(img.encoded) > 0 {
		pdf.RegisterImageOptionsReader(name, opts, bytes.NewReader(img.encoded))
	} else {
		pdf.RegisterImageOptions(name, opts)
	}

	pdf.ImageOptions(name, x, y, w, h, false, opts, 0, "")
}

// gofpdfImageType 返回 gofpdf 能直接按路径读取的图片类型。
//
// gofpdf v1.16.2 只认 jpg / png / gif 三种（既没有 ImageTypeFromExt，
// 也不支持 webp/bmp/tiff）。返回空串表示必须重编码后再注册。
func gofpdfImageType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "jpg"
	case ".png":
		return "png"
	case ".gif":
		return "gif"
	default:
		return ""
	}
}

// ---- PDF 合并 / 加密 ----

// MergePDFs 按入参顺序合并多个 PDF。
func MergePDFs(pdfs []string, output string) error {
	if len(pdfs) == 0 {
		return fmt.Errorf("合并 PDF: 输入列表为空")
	}
	for _, p := range pdfs {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("合并 PDF: 输入 %q 不可读: %w", p, err)
		}
	}
	if err := EnsureDir(filepath.Dir(output)); err != nil {
		return err
	}

	conf := model.NewDefaultConfiguration()
	conf.MergeBookmarkMode = model.MergeBookmarkModePreserve // 不加文件名包装层，书签树直接并入根
	if err := api.MergeCreateFile(pdfs, output, false, conf); err != nil {
		return fmt.Errorf("合并 %d 个 PDF 到 %q 失败: %w", len(pdfs), output, err)
	}
	return nil
}

// SetPDFPassword 给 PDF 设置用户密码。
//
// 所有者密码默认与用户密码相同（原实现硬编码成 "owner5678"，所有者密码是
// 解除权限限制用的，写死一个公开值等于没有权限保护）。
func SetPDFPassword(input, output, password string) error {
	return SetPDFPasswordWithOwner(input, output, password, password)
}

// SetPDFPasswordWithOwner 分别指定用户密码（打开文档用）与所有者密码（解除权限用）。
//
// 只有打印权限：对齐"下载下来自己看"的用途，避免二次分发。
func SetPDFPasswordWithOwner(input, output, userPassword, ownerPassword string) error {
	if userPassword == "" {
		return fmt.Errorf("设置 PDF 密码: 用户密码不能为空")
	}
	if ownerPassword == "" {
		ownerPassword = userPassword
	}
	if err := EnsureDir(filepath.Dir(output)); err != nil {
		return err
	}

	conf := model.NewAESConfiguration(userPassword, ownerPassword, 256)
	conf.Permissions = model.PermissionsPrint
	if err := api.EncryptFile(input, output, conf); err != nil {
		return fmt.Errorf("加密 PDF %q 到 %q 失败: %w", input, output, err)
	}
	return nil
}

// SortImageNames 对文件名做自然排序：数字段按数值比较，所以 "2" 排在 "10" 前面。
//
// 服务端图片名是零填充的（00001.webp），字典序本来就够用；但用户自定义目录里
// 常出现 1.jpg / 2.jpg ... / 10.jpg，字典序会把 10 排到 2 前面。
func SortImageNames(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		return naturalLess(names[i], names[j])
	})
}

// naturalLess 比较两个字符串，把其中的连续数字段按数值大小比较。
func naturalLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		ca, cb := a[ia], b[ib]
		da, db := isDigit(ca), isDigit(cb)
		switch {
		case da && db:
			// 各自取出完整的数字段，去掉前导 0 后比较长度再比较字面值
			na := readNumber(a, &ia)
			nb := readNumber(b, &ib)
			na, nb = strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
		case ca != cb:
			return ca < cb
		default:
			ia++
			ib++
		}
	}
	return len(a)-ia < len(b)-ib
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// readNumber 从 s[*pos] 开始读一段连续数字并推进 *pos。
func readNumber(s string, pos *int) string {
	start := *pos
	for *pos < len(s) && isDigit(s[*pos]) {
		*pos++
	}
	return s[start:*pos]
}

// ---- 占位图 ----

// placeholderPageImage 为读不出来的图片生成一张占位图，保证 PDF 的页码与
// 原作品一致（跳过一张会让后面的页码全部错位）。
//
// 对齐 pdf_test/tools.go 里留下的 TODO：
//
//	//TODO 读取失败的图片要给成占位图（一张纯白照片，水平垂直居中错误信息（读取图片信息失败：图片名））
//
// key 是 gofpdf 的图片注册名，必须唯一，所以用「源文件完整路径 + .placeholder」——
// 不能直接用文件名：一份 PDF 里可能同时合入 a/00001.jpg 和 b/00001.jpg，
// 两张都坏了的话注册名会撞车，gofpdf 会把第二张当成第一张的缓存复用。
//
// displayName 只用于占位图上显示的文字，所以给文件名就够（可读性优先）。
//
// cfg 是已经测出来的尺寸，可能为零值，此时用 A4 比例的默认尺寸。
func placeholderPageImage(key, displayName string, cfg image.Config) (PageImage, error) {
	w, h := cfg.Width, cfg.Height
	if w <= 0 || h <= 0 {
		w, h = 1000, 1414
	}

	img := PlaceholderImage(w, h, fmt.Sprintf("图片读取失败: %s", displayName))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return PageImage{}, fmt.Errorf("编码占位图失败: %w", err)
	}

	return PageImage{
		width:     float64(w),
		height:    float64(h),
		path:      key + ".placeholder",
		bookmark:  bookmarkName(displayName),
		encoded:   buf.Bytes(),
		imageType: "png",
	}, nil
}

// encodeImageForPDF 把 gofpdf 不认识的格式（webp/bmp/tiff/avif）重编码成它能用的格式。
//
// 不透明图走 JPEG（体积小得多），带透明通道的走 PNG（JPEG 会把透明区域变成黑块）。
//
// 注意：按当前流程，还原阶段就已经输出 JPEG（PDF 库不支持 webp），所以这条
// 兜底路径主要服务于"直接拿未还原的原始图合成 PDF"这一类场景。
func encodeImageForPDF(path string) (data []byte, imageType string, err error) {
	img, err := imaging.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("打开图片 %q 失败: %w", path, err)
	}

	buf := &bytes.Buffer{}
	if IsOpaque(img) {
		if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 92}); err != nil {
			return nil, "", fmt.Errorf("编码 JPEG 失败: %w", err)
		}
		return buf.Bytes(), "jpg", nil
	}

	if err := png.Encode(buf, img); err != nil {
		return nil, "", fmt.Errorf("编码 PNG 失败: %w", err)
	}
	return buf.Bytes(), "png", nil
}
