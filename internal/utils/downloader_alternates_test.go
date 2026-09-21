package utils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAlternateCandidateURLsOrderAndDedup 锁定候选直链的顺序与去重。
func TestAlternateCandidateURLsOrderAndDedup(t *testing.T) {
	task := AlternateDownloadTask{
		Url:        "https://a.example/1.webp",
		Dist:       "x",
		Alternates: []string{"https://b.example/1.webp", "https://a.example/1.webp", "", "  "},
	}

	got := task.candidateURLs()
	want := []string{"https://a.example/1.webp", "https://b.example/1.webp"}

	if len(got) != len(want) {
		t.Fatalf("候选数 = %d (%v)，期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// newFallbackTestDownloader 建一个只关心「换链重试」的下载器。
//
// 必须显式关掉图片内容校验（`WithValidateImage(false)`）：校验默认是**开着**的
// （用来拦「线路返回一个错误页 HTML 但状态码 200」这种坑），而本文件喂的是
// 假字节，开着校验会把"好服务器"也判成失败，测出来的就不是换链逻辑了。
// —— 这个坑我先踩过一次：三条用例全红，错误信息还被最后一条失败盖成了
// "连接被拒绝"，看着像服务器没起来。
func newFallbackTestDownloader(maxRetries int) *Downloader {
	return NewDownloader(
		WithMaxRetries(maxRetries),
		WithTimeout(5*time.Second),
		WithValidateImage(false),
	)
}

// deadServer 返回一个已经关掉的 server 地址 —— 连它会立刻 connection refused，
// 用来扮演"域名已下线的 CDN 镜像"。比连 127.0.0.1:9 更可靠（不会挂起）。
func deadServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// TestDownloadFallsBackToAlternate 是本文件的核心：主链指向的镜像已下线时，
// 必须能靠备用直链成功，而不是把整页判死。
//
// 这正是实测踩到的坑：接口下发的图片域名是一批镜像里轮换的，其中有的连 TLS 都
// 握不上手（connection reset），而同一路径在别的镜像上完全正常。
func TestDownloadFallsBackToAlternate(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-image-bytes"))
	}))
	defer good.Close()

	dst := filepath.Join(t.TempDir(), "00001.webp")
	d := newFallbackTestDownloader(1)

	attempts, err := d.DownloadWithAlternates(
		context.Background(),
		deadServer(t)+"/media/photos/1/00001.webp", // 主链：坏镜像
		[]string{good.URL + "/media/photos/1/00001.webp"},
		dst,
	)
	if err != nil {
		t.Fatalf("主链坏、备用链好，应当成功，却报错: %v", err)
	}
	if attempts != 2 {
		t.Errorf("请求次数 = %d，期望 2（主链 1 次 + 备用链 1 次）", attempts)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "fake-image-bytes" {
		t.Errorf("落盘内容不对: %q err=%v", string(b), err)
	}
}

// TestDownloadPrefersPrimary 确认主链可用时不会去碰备用链
// （备用链只是兜底，不该让每次下载都多打一次请求）。
func TestDownloadPrefersPrimary(t *testing.T) {
	var goodHits int
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits++
		_, _ = w.Write([]byte("from-primary"))
	}))
	defer good.Close()

	dst := filepath.Join(t.TempDir(), "00001.webp")
	d := newFallbackTestDownloader(1)

	attempts, err := d.DownloadWithAlternates(
		context.Background(),
		good.URL+"/media/photos/1/00001.webp",
		[]string{deadServer(t) + "/media/photos/1/00001.webp"},
		dst,
	)
	if err != nil {
		t.Fatalf("主链可用却失败: %v", err)
	}
	if attempts != 1 {
		t.Errorf("请求次数 = %d，期望 1（主链一次就成）", attempts)
	}
	if goodHits != 1 {
		t.Errorf("主链被请求了 %d 次，期望 1", goodHits)
	}
	if b, _ := os.ReadFile(dst); string(b) != "from-primary" {
		t.Errorf("落盘内容 = %q，期望来自主链", string(b))
	}
}

// TestDownloadAllCandidatesDead 确认全挂时错误信息点明"几条直链都不可用"，
// 而不是含糊地只说"重试了 N 次" —— 排查时要能一眼看出是域名问题还是内容问题。
func TestDownloadAllCandidatesDead(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "00001.webp")
	d := newFallbackTestDownloader(0)

	attempts, err := d.DownloadWithAlternates(
		context.Background(),
		deadServer(t)+"/media/photos/1/00001.webp",
		[]string{deadServer(t) + "/media/photos/1/00001.webp"},
		dst,
	)
	if err == nil {
		t.Fatal("两条直链都不可用，应当报错")
	}
	if attempts != 2 {
		t.Errorf("请求次数 = %d，期望 2", attempts)
	}
	if !contains(err.Error(), "条直链全部不可用") {
		t.Errorf("错误信息没说明是直链全挂: %v", err)
	}
}

// TestBatchWithAlternatesKeepsIndexOrder 确认结果与入参按下标一一对应。
//
// 这条很要紧：调用方是靠下标把下载结果映射回页码的（download_chapter.go 里
// `downloadErr[plan[i].page]`），错位会让失败原因挂到别的页上。
func TestBatchWithAlternatesKeepsIndexOrder(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok:" + r.URL.Path))
	}))
	defer good.Close()

	dir := t.TempDir()
	tasks := []AlternateDownloadTask{
		{Url: good.URL + "/p1.webp", Dist: filepath.Join(dir, "1.webp")},
		{Url: deadServer(t) + "/p2.webp", Dist: filepath.Join(dir, "2.webp"),
			Alternates: []string{good.URL + "/p2.webp"}},
		{Url: deadServer(t) + "/p3.webp", Dist: filepath.Join(dir, "3.webp")},
	}

	d := newFallbackTestDownloader(0)
	results := d.BatchWithAlternates(context.Background(), tasks, 2, nil)

	if len(results) != 3 {
		t.Fatalf("结果数 = %d，期望 3", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("第 0 条应当成功: %v", results[0].Err)
	}
	if results[1].Err != nil {
		t.Errorf("第 1 条应当靠备用链成功: %v", results[1].Err)
	}
	if results[2].Err == nil {
		t.Error("第 2 条只有坏链，应当失败")
	}
	// Dist 必须原样带回，调用方据此定位文件。
	for i, want := range []string{"1.webp", "2.webp", "3.webp"} {
		if filepath.Base(results[i].Dist) != want {
			t.Errorf("第 %d 条的 Dist = %q，期望 %q", i, results[i].Dist, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
