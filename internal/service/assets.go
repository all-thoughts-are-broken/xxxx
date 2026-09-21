package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// downloaderCache 按「代理 + UA + 超时」复用下载器。
//
// 复用而不是每次新建，关键是为了复用底层 http.Transport 的连接池：
// 一本漫画 40 张图来自同一个 CDN，省掉 40 次 TLS 握手。
var downloaderCache sync.Map // key: string -> *utils.Downloader

// downloaderFor 返回与配置匹配的下载器。
//
// 与 client 一样按指纹复用：代理/UA/超时变了就换一个实例，其余情况命中缓存。
func (s *Service) downloaderFor(cfg *config.Config) *utils.Downloader {
	key := fmt.Sprintf("%s|%s|%d", cfg.Proxy, cfg.UserAgent, cfg.TimeoutSec)
	if v, ok := downloaderCache.Load(key); ok {
		return v.(*utils.Downloader)
	}

	timeout := cfg.Timeout()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	opts := []utils.DownloaderOption{
		utils.WithProxy(cfg.Proxy),
		utils.WithTimeout(timeout),
		utils.WithMaxRetries(cfg.EffectiveMaxRetries()),
		utils.WithSkipExisting(cfg.SkipExisting),
		utils.WithValidateImage(true),
	}
	if cfg.UserAgent != "" {
		opts = append(opts, utils.WithUserAgent(cfg.UserAgent))
	}

	d := utils.NewDownloader(opts...)
	actual, _ := downloaderCache.LoadOrStore(key, d)
	return actual.(*utils.Downloader)
}

// imageDim 读回一张图的宽或高；读失败返回 0。
//
// 只解析文件头（image.DecodeConfig），不解码像素。
func imageDim(path string, wantWidth bool) int {
	cfg, err := utils.DecodeConfigOf(path)
	if err != nil {
		return 0
	}
	if wantWidth {
		return cfg.Width
	}
	return cfg.Height
}
