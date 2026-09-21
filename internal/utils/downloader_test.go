package utils

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestIsDeterministicFailure 锁住「按错误**类型**判断，而不是 grep 错误文案」。
//
// 老实现是在错误信息里找 "状态码 404" 这类子串。问题是文案会改：改一次，
// 4xx 就退化成「被当成临时故障反复重试」—— 这种退化**测试不会红**，
// 只是每次失败都白等 1s / 2s / 4s，非常难被发现。
//
// 顺带这条用例也覆盖了老实现漏掉的 4xx（405/410/402 之类以前会傻等重试）。
func TestIsDeterministicFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404", &statusError{URL: "https://x/00001.webp", StatusCode: http.StatusNotFound}, true},
		{"403", &statusError{URL: "x", StatusCode: http.StatusForbidden}, true},
		{"410（老实现没列举到）", &statusError{URL: "x", StatusCode: http.StatusGone}, true},
		{"451", &statusError{URL: "x", StatusCode: http.StatusUnavailableForLegalReasons}, true},
		{"408 是 4xx 但值得重试", &statusError{URL: "x", StatusCode: http.StatusRequestTimeout}, false},
		{"429 是 4xx 但值得重试", &statusError{URL: "x", StatusCode: http.StatusTooManyRequests}, false},
		{"500 值得重试", &statusError{URL: "x", StatusCode: http.StatusInternalServerError}, false},
		{"内容不是图片", fmt.Errorf("下载: %s %w", "https://x/00001.webp", errNotAnImage), true},
		{"普通网络错误值得重试", errors.New("connection reset by peer"), false},
		{"nil 不 panic", nil, false},
	}
	for _, c := range cases {
		if got := isDeterministicFailure(c.err); got != c.want {
			t.Errorf("%s: isDeterministicFailure = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestIsDeterministicFailureSurvivesWrapping 确认经 %w 包装后判定依然有效。
//
// DownloadWithRetry 会把最后一次错误再包一层（"下载失败（已尝试 N 次）…"）。
// 如果判定改用类型断言而不是 errors.As/Is，这条就会红。
func TestIsDeterministicFailureSurvivesWrapping(t *testing.T) {
	status := &statusError{URL: "https://x/00001.webp", StatusCode: http.StatusNotFound}
	if wrapped := fmt.Errorf("下载失败（已尝试 3 次）%s: %w", "https://x/00001.webp", status); !isDeterministicFailure(wrapped) {
		t.Error("被包装过的 404 应当仍判为确定性失败")
	}

	notImg := fmt.Errorf("下载: %s %w", "https://x/00001.webp", errNotAnImage)
	if wrapped := fmt.Errorf("下载失败（已尝试 3 次）%s: %w", "https://x/00001.webp", notImg); !isDeterministicFailure(wrapped) {
		t.Error("被包装过的「不是图片」应当仍判为确定性失败")
	}
}

// TestDownloadReturnsTypedStatusError 确认 Download 对非 200 回的是结构化错误。
//
// 这是上一条测试的前提：判定逻辑再对，如果 Download 不吐出 *statusError
// 就都白搭。
func TestDownloadReturnsTypedStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	d := NewDownloader()
	err := d.Download(context.Background(), srv.URL+"/00001.webp", filepath.Join(t.TempDir(), "00001.webp"))

	var se *statusError
	if !errors.As(err, &se) {
		t.Fatalf("期望 *statusError，实际 %T: %v", err, err)
	}
	if se.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d，期望 404", se.StatusCode)
	}
}

// TestDownloadRejectsOversizedResponse 确认超限响应会被丢弃、且不留 .part。
func TestDownloadRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		// 写 maxImageBytes+1 个字节：刚好越过上限。
		chunk := make([]byte, 1<<20)
		for written := 0; written <= maxImageBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "00001.webp")
	d := NewDownloader()
	err := d.Download(context.Background(), srv.URL+"/00001.webp", dst)
	if err == nil {
		t.Fatal("超过单图上限的响应应当被拒绝")
	}

	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("超限响应不应在目标路径留下文件: %v", statErr)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("失败路径不应残留任何文件，实际 %d 个", len(entries))
	}
}
