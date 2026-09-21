package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/config"
)

// makeCacheEntry 造一个缓存文件并把修改时间拨到 mtime。返回文件路径。
func makeCacheEntry(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("拨修改时间失败: %v", err)
	}
	return p
}

// TestPruneCacheDirRemovesOnlyExpiredFiles 只删过期的、不删新写的、不碰子目录。
//
// 能让它红的改动：去掉 pruneCacheDir 里的 ModTime().After(cutoff) 判断
// （会误删 fresh），或者把 continue 换成删除（会误删子目录）。
func TestPruneCacheDirRemovesOnlyExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	oldFile := makeCacheEntry(t, dir, "avatar_old.jpg", now.Add(-48*time.Hour))
	freshFile := makeCacheEntry(t, dir, "avatar_fresh.jpg", now.Add(-1*time.Hour))
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("建子目录失败: %v", err)
	}
	subFile := makeCacheEntry(t, sub, "inside.txt", now.Add(-48*time.Hour))

	removed, err := pruneCacheDir(dir, 1, now)
	if err != nil {
		t.Fatalf("pruneCacheDir 报错: %v", err)
	}
	if removed != 1 {
		t.Errorf("应删除 1 个文件，实际 %d", removed)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("过期文件应已被删除")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Errorf("新鲜文件不该被删: %v", err)
	}
	if _, err := os.Stat(subFile); err != nil {
		t.Errorf("子目录里的文件不该被碰: %v", err)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("子目录本身不该被碰: %v", err)
	}
}

// TestPruneCacheDirDisabledByDefault ttl=0（默认值）时必须是彻底的 no-op，
// 哪怕目录里全是几百天前的老文件。
//
// 能让它红的改动：去掉 pruneCacheDir 开头的 ttlDays <= 0 短路。
func TestPruneCacheDirDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	ancient := makeCacheEntry(t, dir, "emoji_old.png", now.Add(-720*time.Hour))

	removed, err := pruneCacheDir(dir, 0, now)
	if err != nil {
		t.Fatalf("pruneCacheDir 报错: %v", err)
	}
	if removed != 0 {
		t.Errorf("ttl=0 不应删除任何文件，实际删了 %d", removed)
	}
	if _, err := os.Stat(ancient); err != nil {
		t.Errorf("ttl=0 时老文件不该被删: %v", err)
	}
	if got := config.Default().CacheTTLDays; got != 0 {
		t.Errorf("默认配置里 cache_ttl_days 必须是 0（关闭），实际 %d", got)
	}
}

// TestPruneCacheDirMissingDirIsNoop 目录不存在不该报错 —— 首次运行时
// output/.cache 还没被创建过。
func TestPruneCacheDirMissingDirIsNoop(t *testing.T) {
	removed, err := pruneCacheDir(filepath.Join(t.TempDir(), "not-exist"), 7, time.Now())
	if err != nil {
		t.Errorf("目录不存在应视为 no-op 而非错误: %v", err)
	}
	if removed != 0 {
		t.Errorf("不存在目录应返回 0，实际 %d", removed)
	}
}
