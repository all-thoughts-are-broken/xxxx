package service

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/config"
)

// PruneCacheAtStartup 按 cache_ttl_days 清理资源缓存目录，在进程启动时调用一次。
//
// 只在有删除发生时打日志（stderr）。任何失败都只记日志、不中断启动 ——
// 缓存清理是锦上添花，不值得为它让整个进程起不来。
func PruneCacheAtStartup(cfg *config.Config) {
	removed, err := pruneCacheDir(cacheDir(cfg), cfg.CacheTTLDays, time.Now())
	if err != nil {
		log.Printf("[cache] 清理缓存目录失败（不影响启动）: %v", err)
		return
	}
	if removed > 0 {
		log.Printf("[cache] 按缓存有效期（%d 天）删除了 %d 个过期文件", cfg.CacheTTLDays, removed)
	}
}

// pruneCacheDir 删除 dir 下修改时间早于 now-ttlDays 的顶层文件，返回删除数。
//
// 缓存目录是扁平的（cover_* / avatar_* / badge_* / emoji_*），只扫顶层；
// 子目录一律不碰，万一未来缓存出现子结构也不会误删。ttlDays <= 0 时不做任何事。
//
// 并发约束：只能在进程启动、尚无任何渲染在跑时调用。渲染层按路径直接读取
// 这些缓存文件，运行中删除会与在途请求竞争（先校验存在、后打开之间被删走，
// 渲染就会拿到一个读不到的路径）。
func pruneCacheDir(dir string, ttlDays int, now time.Time) (int, error) {
	if ttlDays <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 目录还不存在 = 无缓存可清，不是错误
		}
		return 0, err
	}
	cutoff := now.AddDate(0, 0, -ttlDays)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
		// 删除失败（比如被并发方抢先删掉）不计数也不报错，下一轮启动再说。
	}
	return removed, nil
}
