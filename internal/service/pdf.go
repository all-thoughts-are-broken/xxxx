package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// ConvertToPDFRequest 是「把图片合成 PDF」的请求。
//
// 图片来源二选一：Images（显式列表，顺序即页序）或 InputDir（目录，按自然序）。
// 两者都为空时报错。
type ConvertToPDFRequest struct {
	// Images 显式图片路径列表，顺序即 PDF 页序。
	Images []string `json:"images"`
	// InputDir 图片目录；取其中的图片文件（不递归）并按自然序排序。
	InputDir string `json:"input_dir"`
	// OutputPath 输出 PDF 路径；为空时自动命名到 output_dir。
	OutputPath string `json:"output_path"`

	// Chapter 顶层书签名；为空则不建顶层书签。
	Chapter string `json:"chapter"`
	// Layout single（整本一条长页，默认）/ paged（按 MaxPageHeight 分页）。
	Layout string `json:"layout"`
	// PerImageBookmark 是否为每张图建二级书签。
	//
	// 用指针是为了区分「没传」和「显式传 false」：没传时跟随配置
	// （pdf_per_image_bookmark，默认 true）。
	PerImageBookmark *bool `json:"per_image_bookmark"`
	// MaxPageHeight 单页最大高度（pt）；<=0 表示不限制。
	MaxPageHeight float64 `json:"max_page_height"`

	// Password 用户密码；为空则不加密。
	Password string `json:"password"`
	// OwnerPassword 所有者密码；为空时与 Password 相同。
	OwnerPassword string `json:"owner_password"`
}

// PDFResult 是 PDF 类操作的结果。
type PDFResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Pages int    `json:"pages"`
	// Encrypted 报告结果是否加密。
	Encrypted bool `json:"encrypted"`
	// SourceCount 参与合成的图片数量。
	SourceCount int `json:"source_count"`
}

// ConvertImagesToPDF 把图片合成 PDF（可选加密）。
func (s *Service) ConvertImagesToPDF(ctx context.Context, prog ProgressFunc, req ConvertToPDFRequest) (*PDFResult, error) {
	cfg := s.Config()

	images, err := s.resolveImages(req)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		// 把「没给目录」和「目录里没图」分开报：两者对调用方是完全不同的处置动作
		// （补参数 vs 检查上游步骤是否跑完）。合并成一句 "至少要给一个" 会让
		// 「目录确实给了、只是还空着」看起来像参数写错了。
		if dir := strings.TrimSpace(req.InputDir); dir != "" {
			return nil, protocol.Errorf(protocol.CodeNotFound, "目录 %s 里没有找到任何图片", dir)
		}
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"没有可用的图片：images 与 input_dir 至少要给一个")
	}

	layout := strings.TrimSpace(req.Layout)
	if layout == "" {
		layout = cfg.PDFLayout
	}

	out := strings.TrimSpace(req.OutputPath)
	if out == "" {
		out = filepath.Join(cfg.OutputDir, "merged.pdf")
	}

	// 未显式指定时，沿用配置里的页高上限（默认 0 = 不限制，
	// 对应「一章 = 一个只有一页的长 PDF」这个产品形态）。
	maxH := req.MaxPageHeight
	if maxH <= 0 {
		maxH = cfg.PDFMaxPageHeight
	}

	prog.report(Progress{
		Stage:   "pdf",
		Total:   len(images),
		Message: "合成 PDF（" + itoa(len(images)) + " 张图）",
	})

	// 未显式指定时跟随配置默认（默认 true）。理由：同一套产物，
	// 不该出现「下载出来的有页码书签、手动转出来的没有」这种不一致。
	perImageBookmark := cfg.PDFPerImageBookmark
	if req.PerImageBookmark != nil {
		perImageBookmark = *req.PerImageBookmark
	}

	if err := utils.GeneratePdfFromImages(images, out, utils.PDFOptions{
		Chapter:          strings.TrimSpace(req.Chapter),
		MaxPageHeight:    maxH,
		Layout:           layout,
		PerImageBookmark: perImageBookmark,
	}); err != nil {
		return nil, protocol.Wrap(protocol.CodeIO, err, "合成 PDF 失败")
	}

	res := &PDFResult{Path: out, SourceCount: len(images)}
	if st, err := os.Stat(out); err == nil {
		res.Bytes = st.Size()
	}

	if req.Password != "" {
		prog.report(Progress{Stage: "encrypt", Message: "正在加密 PDF"})
		if _, err := encryptInPlace(out, req.Password, req.OwnerPassword); err != nil {
			return res, err
		}
		res.Encrypted = true
		if st, err := os.Stat(out); err == nil {
			res.Bytes = st.Size()
		}
	}

	return res, nil
}

// resolveImages 解析出参与合成的图片列表。
//
// 显式列表优先：它允许调用方指定任意顺序、任意来源。
// 只给目录时按自然序排序（00001 / 00002 ... 或 1 / 2 / 10 都能排对），
// 并跳过非图片文件（.DS_Store、说明文本等）。
func (s *Service) resolveImages(req ConvertToPDFRequest) ([]string, error) {
	if len(req.Images) > 0 {
		out := make([]string, 0, len(req.Images))
		for _, p := range req.Images {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !utils.IsImageFile(p) {
				return nil, protocol.Errorf(protocol.CodeInvalidParams, "不是支持的图片格式: %s", p)
			}
			out = append(out, p)
		}
		return out, nil
	}

	dir := strings.TrimSpace(req.InputDir)
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, protocol.Wrap(protocol.CodeIO, err, "读取图片目录 %s 失败", dir)
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

	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(dir, n))
	}
	return out, nil
}

// MergePDFRequest 是合并 PDF 的请求。
type MergePDFRequest struct {
	// Inputs 待合并的 PDF 列表，顺序即结果顺序。
	Inputs []string `json:"inputs"`
	// OutputPath 输出路径。
	OutputPath string `json:"output_path"`
	// Password 合并后加密用的密码；为空则不加密。
	Password      string `json:"password"`
	OwnerPassword string `json:"owner_password"`
}

// MergePDFs 合并多个 PDF（可选加密）。
func (s *Service) MergePDFs(ctx context.Context, prog ProgressFunc, req MergePDFRequest) (*PDFResult, error) {
	cfg := s.Config()

	inputs := make([]string, 0, len(req.Inputs))
	for _, p := range req.Inputs {
		if p = strings.TrimSpace(p); p != "" {
			inputs = append(inputs, p)
		}
	}
	if len(inputs) == 0 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "待合并的 PDF 列表为空")
	}

	out := strings.TrimSpace(req.OutputPath)
	if out == "" {
		out = filepath.Join(cfg.OutputDir, "merged.pdf")
	}

	prog.report(Progress{
		Stage:   "merge",
		Total:   len(inputs),
		Message: "合并 " + itoa(len(inputs)) + " 个 PDF",
	})

	if err := utils.MergePDFs(inputs, out); err != nil {
		return nil, protocol.Wrap(protocol.CodeIO, err, "合并 PDF 失败")
	}

	res := &PDFResult{Path: out, SourceCount: len(inputs)}
	if st, err := os.Stat(out); err == nil {
		res.Bytes = st.Size()
	}

	if req.Password != "" {
		prog.report(Progress{Stage: "encrypt", Message: "正在加密 PDF"})
		if _, err := encryptInPlace(out, req.Password, req.OwnerPassword); err != nil {
			return res, err
		}
		res.Encrypted = true
		if st, err := os.Stat(out); err == nil {
			res.Bytes = st.Size()
		}
	}
	return res, nil
}

// EncryptPDFRequest 是加密 PDF 的请求。
type EncryptPDFRequest struct {
	InputPath     string `json:"input_path"`
	OutputPath    string `json:"output_path"`
	Password      string `json:"password"`
	OwnerPassword string `json:"owner_password"`
}

// EncryptPDF 给已有 PDF 加密码。
func (s *Service) EncryptPDF(ctx context.Context, prog ProgressFunc, req EncryptPDFRequest) (*PDFResult, error) {
	in := strings.TrimSpace(req.InputPath)
	if in == "" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "input_path 不能为空")
	}
	if strings.TrimSpace(req.Password) == "" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "password 不能为空")
	}

	out := strings.TrimSpace(req.OutputPath)
	if out == "" {
		out = in // 就地加密
	}

	prog.report(Progress{Stage: "encrypt", Message: "正在加密 PDF"})

	res := &PDFResult{Path: out, SourceCount: 1}
	if samePath(in, out) {
		if _, err := encryptInPlace(in, req.Password, req.OwnerPassword); err != nil {
			return res, err
		}
	} else {
		if err := utils.SetPDFPasswordWithOwner(in, out, req.Password, req.OwnerPassword); err != nil {
			return res, protocol.Wrap(protocol.CodeIO, err, "加密 PDF 失败")
		}
	}

	res.Encrypted = true
	if st, err := os.Stat(out); err == nil {
		res.Bytes = st.Size()
	}
	return res, nil
}

// samePath 判断两个路径是否指向同一个文件（大小写与分隔符归一后比较）。
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(absA), filepath.Clean(absB))
}
