package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
)

// ---- 「空数组 = 对象不存在」这个服务端约定 ----

// TestIsEmptyJSONArray 只认真正的空数组。
//
// 不能把非空数组也当「不存在」：那说明接口形态变了，属于真异常，
// 得让它继续走解析失败，别被悄悄吞成 not_found。
func TestIsEmptyJSONArray(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"[]", true},
		{"  []  ", true},
		{"[]\n", true},
		{"[ ]", false}, // 中间有空格：不认，宁可走解析失败
		{"[{}]", false},
		{"[]{}", false},
		{"{}", false},
		{"", false},
		{"[", false},
	}
	for _, c := range cases {
		if got := isEmptyJSONArray([]byte(c.in)); got != c.want {
			t.Errorf("isEmptyJSONArray(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// TestDecodeObjectMapsEmptyArrayToNotFound 空数组要变成 not_found。
//
// 背景：GET /album 与 GET /comic_read 在对象不存在时返回 `[]`（2026-09-21 实测）。
// 修复前它会掉进 json.Unmarshal，报 `internal: 解析响应 JSON 失败 ...
// cannot unmarshal array` —— 宿主看到 internal 会以为工具坏了，实际只是作品号
// 填错了。这条测试锁住「错误码必须是 not_found」。
func TestDecodeObjectMapsEmptyArrayToNotFound(t *testing.T) {
	var v struct {
		Id int `json:"id"`
	}
	err := decodeObject([]byte("[]"), &v, "/album?id=500000")
	if err == nil {
		t.Fatal("空数组应当被识别为「对象不存在」")
	}

	if code := protocol.CodeOf(err); code != protocol.CodeNotFound {
		t.Errorf("错误码 = %q，期望 %q", code, protocol.CodeNotFound)
	}
	if !errors.Is(err, errEmptyArray) {
		t.Error("错误链里应当能 errors.Is 到 errEmptyArray（上层据此换成人话）")
	}
	if !strings.Contains(err.Error(), "/album?id=500000") {
		t.Errorf("错误信息该带上端点便于定位，实际: %s", err.Error())
	}
}

// TestDecodeObjectKeepsNormalPath 正常对象照常解析。
func TestDecodeObjectKeepsNormalPath(t *testing.T) {
	var v struct {
		Id int `json:"id"`
	}
	if err := decodeObject([]byte(`{"id":42}`), &v, "/album?id=42"); err != nil {
		t.Fatalf("正常对象不该报错: %v", err)
	}
	if v.Id != 42 {
		t.Errorf("Id = %d，期望 42", v.Id)
	}
}

// TestDecodeObjectRejectsNonEmptyArray 非空数组仍按解析失败处理。
func TestDecodeObjectRejectsNonEmptyArray(t *testing.T) {
	var v struct {
		Id int `json:"id"`
	}
	err := decodeObject([]byte(`[{"id":1}]`), &v, "/album?id=1")
	if err == nil {
		t.Fatal("非空数组应当报错（接口形态变了，是真异常）")
	}
	if code := protocol.CodeOf(err); code != protocol.CodeInternal {
		t.Errorf("非空数组的错误码 = %q，期望 internal", code)
	}
}

// ---- 重定向 ----

// TestClientDoesNotFollowRedirect 锁住「签名不随重定向外发」。
//
// token / tokenparam 是通过**自定义头**发出去的，而 net/http 只在跨域重定向时
// 剥掉 Authorization / Cookie，**不会**动自定义头。所以一旦跟随跳转，签名就
// 送给了第三方。实测正常线路不返回 302，所以不跟随不影响功能。
//
// 断言方式是数「重定向目标被访问了几次」—— 这是最直接的证据：只要它被访问过，
// 签名就已经发出去了。
func TestClientDoesNotFollowRedirect(t *testing.T) {
	var targetHits atomic.Int64

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/album?id=1", http.StatusFound)
	}))
	defer origin.Close()

	c, err := NewClient(
		WithBaseURL(origin.URL),
		WithSecret("test-secret"),
		WithAppVersion("1.0.0"),
	)
	if err != nil {
		t.Fatalf("NewClient 失败: %v", err)
	}

	if _, err := c.AlbumDetail(context.Background(), 1); err == nil {
		t.Fatal("3xx 应当被当作错误返回，而不是闷声跟着走")
	}

	if n := targetHits.Load(); n != 0 {
		t.Errorf("重定向目标被访问了 %d 次 —— 签名头已经跟着发出去了", n)
	}
}
