package service

import (
	"strings"
	"testing"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
)

// TestAlternateURLsOnlySwapsHost 锁定备用直链的生成规则：**只换域名**。
//
// 这条规则是实测得来的：图片路径（/media/photos/<aid>/<page:05>.webp）与域名无关，
// 同一路径在多个镜像上返回完全相同的字节。所以一旦某个镜像域名下线，
// 把 host 换掉就能拿到同一张图 —— 不需要重新请求接口，也不需要知道文件名规则。
func TestAlternateURLsOnlySwapsHost(t *testing.T) {
	primary := "https://dead.example/media/photos/1473386/00001.webp?t=1789606478"
	got := alternateURLs(primary, []string{"good.example", "dead.example", "other.example"})

	if len(got) != 2 {
		t.Fatalf("备用链数量 = %d (%v)，期望 2（与主链同域名的要被跳过）", len(got), got)
	}
	want := []string{
		"https://good.example/media/photos/1473386/00001.webp?t=1789606478",
		"https://other.example/media/photos/1473386/00001.webp?t=1789606478",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %q，期望 %q", i, got[i], want[i])
		}
	}

	// 路径与 query 必须原样保留，只换 host。
	for _, u := range got {
		if !strings.Contains(u, "/media/photos/1473386/00001.webp?t=1789606478") {
			t.Errorf("备用链 %q 的路径/query 被改动了", u)
		}
	}
}

func TestAlternateURLsEdgeCases(t *testing.T) {
	primary := "https://a.example/x/00001.webp"

	cases := []struct {
		name  string
		url   string
		hosts []string
		want  int
	}{
		{"没有候选域名", primary, nil, 0},
		{"候选只有自己", primary, []string{"a.example"}, 0},
		{"候选全是自己", primary, []string{"a.example", "A.EXAMPLE"}, 0},
		{"URL 解析不了", "://bad", []string{"b.example"}, 0},
		{"URL 没有 host", "/media/x.webp", []string{"b.example"}, 0},
		{"空串", "", []string{"b.example"}, 0},
		{"候选带空格", primary, []string{"  b.example  "}, 1},
	}
	for _, c := range cases {
		if got := alternateURLs(c.url, c.hosts); len(got) != c.want {
			t.Errorf("%s: 得到 %d 条 (%v)，期望 %d", c.name, len(got), got, c.want)
		}
	}
}

// TestAlternateImageHostsOrderAndDedup 确认候选域名来自服务端自己给的信息：
// 探活学到的优先，然后是本章直链里出现过的其它域名，去重、不硬编码。
func TestAlternateImageHostsOrderAndDedup(t *testing.T) {
	page := &client.ReadPageResult{
		Images: []client.Images{
			{Page: 1, Image: "https://dead.example/media/photos/1/00001.webp"},
			{Page: 2, Image: "https://mirror.example/media/photos/1/00002.webp"},
			{Page: 3, Image: "https://dead.example/media/photos/1/00003.webp"},
			{Page: 4, Image: ""},
		},
	}

	got := alternateImageHosts("probe.example", page)
	want := []string{"probe.example", "dead.example", "mirror.example"}

	if len(got) != len(want) {
		t.Fatalf("域名数 = %d (%v)，期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 = %q，期望 %q（全量 %v）", i, got[i], want[i], got)
		}
	}
}

func TestAlternateImageHostsWithoutPage(t *testing.T) {
	// 阅读页为 nil（理论上不会发生）时不该 panic，至少要返回探活学到的那个。
	got := alternateImageHosts("probe.example", nil)
	if len(got) != 1 || got[0] != "probe.example" {
		t.Errorf("得到 %v，期望 [probe.example]", got)
	}

	// preferred 为空时不该塞进空串。
	got = alternateImageHosts("", nil)
	if len(got) != 0 {
		t.Errorf("得到 %v，期望空", got)
	}
}
