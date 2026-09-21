package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/all-thoughts-are-broken/xxxx/internal/config"
)

// writeTestFile 写一个测试文件，父目录不存在时自动创建。
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// ---- 删除逻辑（最危险的一块：宁可留垃圾也不能误删） ----

// TestRemoveDirIfOnlyOursDeletesPureImageDir 纯产物目录应当被清掉。
func TestRemoveDirIfOnlyOursDeletesPureImageDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "1472136")
	writeTestFile(t, filepath.Join(dir, "00001.jpg"), "a")
	writeTestFile(t, filepath.Join(dir, "00002.png"), "b")

	removeDirIfOnlyOurs(dir)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("纯图片目录应当被删除，实际仍在（err=%v）", err)
	}
}

// TestRemoveDirIfOnlyOursKeepsMixedDir 混了非图片文件时**一个都不删**。
//
// 这是「不误删用户文件」的最后一道防线。注意断言是"图片也不能删" ——
// 保守到这种程度是刻意的：只删干净整目录，不做部分删除。
// 如果有人把它改成 os.RemoveAll 或"删图片留其它"，这条会红。
func TestRemoveDirIfOnlyOursKeepsMixedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mixed")
	img := filepath.Join(dir, "00001.jpg")
	keep := filepath.Join(dir, "我的笔记.txt")
	writeTestFile(t, img, "a")
	writeTestFile(t, keep, "别删我")

	removeDirIfOnlyOurs(dir)

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("非图片文件被误删: %v", err)
	}
	if _, err := os.Stat(img); err != nil {
		t.Errorf("混目录时连图片也不该删（要么整体删、要么整体留）: %v", err)
	}
}

// TestRemoveDirIfOnlyOursKeepsDirWithSubdir 有子目录时直接放弃。
func TestRemoveDirIfOnlyOursKeepsDirWithSubdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "withsub")
	writeTestFile(t, filepath.Join(dir, "00001.jpg"), "a")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	removeDirIfOnlyOurs(dir)

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("有子目录时不该删: %v", err)
	}
}

// TestRemoveDirIfOnlyOursMissingDir 目录不存在时安静返回（不 panic）。
func TestRemoveDirIfOnlyOursMissingDir(t *testing.T) {
	removeDirIfOnlyOurs(filepath.Join(t.TempDir(), "根本就没有这个目录"))
}

// TestClearDirsHidesDeletedPaths 目录已删掉时不该把路径回报给宿主。
func TestClearDirsHidesDeletedPaths(t *testing.T) {
	chapters := []ChapterResult{
		{RawDir: "/raw/a", ImageDir: "/img/a"},
		{RawDir: "/raw/b", ImageDir: "/img/b"},
	}

	got := clearRawDirs(chapters)
	for _, ch := range got {
		if ch.RawDir != "" {
			t.Errorf("clearRawDirs 后 RawDir 应为空，实际 %q", ch.RawDir)
		}
		if ch.ImageDir == "" {
			t.Error("clearRawDirs 不该动 ImageDir")
		}
	}

	got = clearImageDirs(got)
	for _, ch := range got {
		if ch.ImageDir != "" {
			t.Errorf("clearImageDirs 后 ImageDir 应为空，实际 %q", ch.ImageDir)
		}
	}
}

// ---- 落盘 ----

// TestCopyFileIsAtomicAndFaithful 内容一致、自动建父目录、不留 .part。
func TestCopyFileIsAtomicAndFaithful(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.gif")
	writeTestFile(t, src, "GIF89a-fake-bytes")
	dst := filepath.Join(dir, "out", "00001.gif") // 父目录故意不存在

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile 失败: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读回目标失败: %v", err)
	}
	if string(got) != "GIF89a-fake-bytes" {
		t.Errorf("内容不一致: %q", got)
	}
	if names := dirEntries(t, filepath.Dir(dst)); len(names) != 1 {
		t.Errorf("目标目录应当只有 1 个成品，实际 %d 个: %v", len(names), names)
	}
}

// TestCopyFileMissingSource 源不存在时报错，且不在目标留下半截文件。
func TestCopyFileMissingSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.gif")

	if err := copyFile(filepath.Join(dir, "没有这个文件"), dst); err == nil {
		t.Error("源文件不存在时应当报错")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("失败时不该留下目标文件: %v", err)
	}
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Errorf("失败后目录应当为空，实际: %v", names)
	}
}

// TestFileExists 区分文件与目录（下游用它判断"这页好了没"）。
func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.jpg")
	writeTestFile(t, f, "x")

	if !fileExists(f) {
		t.Error("存在的文件应当为 true")
	}
	if fileExists(filepath.Join(dir, "没有这个")) {
		t.Error("不存在的路径应当为 false")
	}
	if fileExists(dir) {
		t.Error("目录应当为 false（语义是「是个文件」）")
	}
}

// ---- 失败明细的截断 ----

// TestAddFailureCapsAndCountsDropped 超过上限后只计数不追加。
//
// 一本 200 页的漫画整本站挂掉时，逐条回传会把结果帧撑到几百 KB。
func TestAddFailureCapsAndCountsDropped(t *testing.T) {
	var res ChapterResult
	const extra = 5
	for i := 0; i < maxReportedFailures+extra; i++ {
		addFailure(&res, ImageFailure{Page: i + 1, URL: "https://x", Stage: "download", Error: "boom"})
	}

	if len(res.Failures) != maxReportedFailures {
		t.Errorf("Failures 应当被截到 %d 条，实际 %d", maxReportedFailures, len(res.Failures))
	}
	if res.DroppedFailures != extra {
		t.Errorf("DroppedFailures = %d，期望 %d", res.DroppedFailures, extra)
	}
}

// TestMsgOf nil 返回空串，不返回 "<nil>"。
func TestMsgOf(t *testing.T) {
	if got := msgOf(nil); got != "" {
		t.Errorf("msgOf(nil) = %q，期望空串", got)
	}
}

// ---- 文件名兜底 ----

// TestSanitizeOrFallsBackForUselessNames 清洗后没有实际内容时走兜底。
//
// 纯点号/纯分隔符这类名字必须落到 fallback：`..` 与 `...` 在 Windows 上
// 会被文件系统解释成父目录，直接拿去 Join 路径就是越界。
func TestSanitizeOrFallsBackForUselessNames(t *testing.T) {
	cases := []struct{ in, want string }{
		{"正常名字", "正常名字"},
		{"", "fb"},
		{"   ", "fb"},
		{"...", "fb"},
		{"//", "fb"},
		{"..", "fb"},
		{"a/b", "a_b"},
	}
	for _, c := range cases {
		if got := sanitizeOr(c.in, "fb"); got != c.want {
			t.Errorf("sanitizeOr(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestCacheDirDefaults 输出目录为空时不该拼出一个裸的 ".cache"。
func TestCacheDirDefaults(t *testing.T) {
	if got := cacheDir(&config.Config{}); got != filepath.Join("output", ".cache") {
		t.Errorf("cacheDir(空) = %q", got)
	}
	if got := cacheDir(&config.Config{OutputDir: "G:/out"}); got != filepath.Join("G:/out", ".cache") {
		t.Errorf("cacheDir(G:/out) = %q", got)
	}
}
