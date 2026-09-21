package utils

import (
	"fmt"
	"image"
	"os"

	// 注册 webp 解码器。
	//
	// 这一行是必需的：imaging v1.6.2 内部只 import 了 bmp/tiff/gif/jpeg/png，
	// 而 imaging.Open 走的是标准库的 image.Decode，所以没有这个注册，
	// 所有 .webp 来源的图片都会报 "image: unknown format"。
	_ "golang.org/x/image/webp"
)

// ImageExts 是本地可能遇到的图片扩展名集合（小写、带点）。
var ImageExts = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".bmp": {},
	".tif": {}, ".tiff": {}, ".webp": {}, ".avif": {},
}

// IsImageFile 判断路径是否是图片（只看扩展名）。
func IsImageFile(path string) bool {
	_, ok := ImageExts[extOf(path)]
	return ok
}

// extOf 返回小写扩展名。
func extOf(path string) string {
	for i := len(path) - 1; i >= 0 && path[i] != '/' && path[i] != '\\'; i-- {
		if path[i] == '.' {
			e := path[i:]
			out := make([]byte, len(e))
			for j := range e {
				c := e[j]
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				out[j] = c
			}
			return string(out)
		}
	}
	return ""
}

// DecodeConfigOf 只读取图片头部拿到宽高，不解码整张图。
//
// 这里的开销是常数级，适合在生成 PDF 前批量测量尺寸；遇到 decodable 之外的
// 格式（比如 avif）会失败，调用方自行决定是否降级为完整解码。
func DecodeConfigOf(path string) (image.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, fmt.Errorf("打开图片 %q 失败: %w", path, err)
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return image.Config{}, fmt.Errorf("解析图片头 %q 失败: %w", path, err)
	}
	return cfg, nil
}

// IsOpaque 报告图片是否完全不透明（用于决定重编码成 JPEG 还是 PNG）。
func IsOpaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return true
}
