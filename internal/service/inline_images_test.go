package service

import (
	"strings"
	"testing"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// TestCollectInlineImagesWalksOneReplyLevel 锁定预抓范围：顶层评论 + 一层楼中楼。
//
// 楼中楼只下钻一层是有意的：接口的 replys 里没有更深的结构，而 client.Reply
// 本身也没有 Replys 字段，多写的递归只会是永不执行的分支。
func TestCollectInlineImagesWalksOneReplyLevel(t *testing.T) {
	stickerA := "https://www.cdnbea.net/media/emoji/a.png"
	stickerB := "https://www.cdnbea.net/media/emoji/b.png"
	replys := []client.Reply{
		{CID: "2", Content: `<div><img src="` + stickerB + `" alt="生病" /></div>`},
		{CID: "3", Content: "手打的😋"},
	}
	comments := []client.List{
		{CID: "1", Content: `<div><img src="` + stickerA + `" alt="惊喜" /></div>`},
		// 同一张贴纸又出现一次：一页评论里重复用同一张很常见，必须只抓一次。
		{CID: "4", Content: `<div><img src="` + stickerA + `" /></div>`, Replys: &replys},
		{CID: "5", Content: "纯文字，没有表情"},
	}

	got := collectInlineImages(comments)
	want := []struct {
		url     string
		sticker bool
	}{
		{stickerA, true},
		{stickerB, true},
		{utils.TwemojiURL("1f60b"), false},
	}
	if len(got) != len(want) {
		t.Fatalf("收集到 %d 条，期望 %d 条: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].URL != want[i].url || got[i].Sticker != want[i].sticker {
			t.Errorf("第 %d 条 = %+v，期望 url=%s sticker=%v", i, got[i], want[i].url, want[i].sticker)
		}
	}
}

// TestInlineAlternatesOnlyForStickers 锁定「只有服务端贴纸才换镜像域名」。
//
// 贴纸与漫画图片同源，路径与域名无关，换域名确实能拿到同一张图（见
// alternateURLs 的实测注释）；Twemoji 在第三方 CDN 上，套上 JM 的域名
// 只会白等一轮失败。
func TestInlineAlternatesOnlyForStickers(t *testing.T) {
	cfg := &config.Config{CDNHost: "cdn-msp.jmapiproxy3.cc"}

	alt := inlineAlternates(cfg, utils.InlineImage{
		URL:     "https://www.cdnbea.net/media/emoji/a.png",
		Sticker: true,
	})
	if len(alt) != 1 || !strings.HasPrefix(alt[0], "https://cdn-msp.jmapiproxy3.cc/media/emoji/a.png") {
		t.Errorf("贴纸的备用链 = %v，期望换成 CDN 域名", alt)
	}

	if alt := inlineAlternates(cfg, utils.InlineImage{URL: utils.TwemojiURL("1f60b")}); alt != nil {
		t.Errorf("Twemoji 不该有备用域名，得到 %v", alt)
	}
	if alt := inlineAlternates(&config.Config{}, utils.InlineImage{URL: "https://x/a.png", Sticker: true}); alt != nil {
		t.Errorf("还没探到 CDN 域名时不该编造备用链，得到 %v", alt)
	}
}

// TestURLHashStableAndDistinct 锁缓存文件名。
//
// 按地址算、跨次运行稳定，才能命中下载器的 skip_existing —— 表情图每次渲染
// 都要用，重下就是白打几百次请求。
func TestURLHashStableAndDistinct(t *testing.T) {
	a := urlHash("https://x/media/emoji/a.png")
	if a != urlHash("https://x/media/emoji/a.png") {
		t.Errorf("同一地址两次哈希不一致: %s", a)
	}
	if a == urlHash("https://x/media/emoji/b.png") {
		t.Errorf("不同地址哈希相同: %s", a)
	}
	if len(a) != 32 {
		t.Errorf("哈希长度 = %d，期望 32（md5 十六进制）", len(a))
	}
}

func TestMediaExt(t *testing.T) {
	cases := map[string]string{
		"https://x/media/emoji/aa.png":         ".png",
		"https://x/media/emoji/aa.png?t=12":    ".png",
		"https://x/media/emoji/aa":             ".png",
		"https://x/media/emoji/aa.webp?v=1":    ".webp",
		"https://x/media/emoji/aa.verylongext": ".png",
	}
	for in, want := range cases {
		if got := mediaExt(in); got != want {
			t.Errorf("mediaExt(%q) = %q，期望 %q", in, got, want)
		}
	}
}
