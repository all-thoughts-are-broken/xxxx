package utils

import (
	"image"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/fogleman/gg"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// resetFontCaches 清空字体缓存，强制后续渲染走「冷缓存填充」路径。
//
// 只在测试里用：缓存已热时 face() 直接命中，不会碰写路径，竞态就藏起来了。
// 必须在没有渲染在跑的时候调用（测试主 goroutine 里、起 goroutine 之前）。
func resetFontCaches() {
	fontFaces = map[string]*opentype.Font{}
	faceCache = map[faceKey]font.Face{}
	ascentCache = map[faceKey]float64{}
	faceCacheDcs = map[float64]*gg.Context{}
}

// TestConcurrentRenderIsSafe 锁住「渲染可以并发调用」这个契约。
//
// 背景：字体缓存是无锁包级 map，协议层又默认允许 8 个请求同时在跑，
// 宿主完全可以同时下发详情卡与评论图。修之前这条测试会直接打印
//
//	fatal error: concurrent map writes
//
// 然后整个测试进程消失 —— 这是运行时的 fatal，不是 panic，业务侧的
// recover 接不住。所以这条测试**不需要 -race 也能抓到回归**：
// 一旦有人把 renderMu 去掉，进程就没了。
//
// 断言取两层：全部调用无错误，且每张产出的 PNG 都能解码出合理尺寸
// （只断言"没崩"是不够的，串行化若写出错图同样是坏的）。
func TestConcurrentRenderIsSafe(t *testing.T) {
	withTestFonts(t)
	resetFontCaches()

	dir := t.TempDir()

	const n = 8
	outs := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = filepath.Join(dir, "out_"+strconv.Itoa(i)+".png")
			if i%2 == 0 {
				errs[i] = RenderCommentPage("评论区", "共 1 条", []Comment{
					{Nickname: "测试", Content: "你好世界 hello", Level: 3, LevelName: "初来乍到"},
				}, outs[i])
				return
			}
			errs[i] = RenderAlbumCard(AlbumCard{
				ID:      "1472136",
				Name:    "并发测试作品",
				Desc:    "简介",
				Tags:    []string{"标签"},
				Author:  []string{"作者"},
				Series:  2,
				Reading: 1,
			}, outs[i])
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("第 %d 个渲染报错: %v", i, errs[i])
			continue
		}
		cfg, err := decodePNGConfig(outs[i])
		if err != nil {
			t.Errorf("第 %d 个产物 %q 不可解码: %v", i, outs[i], err)
			continue
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			t.Errorf("第 %d 个产物尺寸异常: %dx%d", i, cfg.Width, cfg.Height)
		}
	}
}

// decodePNGConfig 只读图片头部取尺寸，避免测试里整图解码。
func decodePNGConfig(path string) (image.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	return cfg, err
}
