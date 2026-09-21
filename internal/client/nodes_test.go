package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// nodeListFixture 是一份**真实拉到的**线路清单密文
// （newsvr-2025.txt，2026-09-21 取自北京镜像）。
//
// 用真实密文而不是自己加密一串当夹具，能同时锁住三件事：清单盐没写错、
// AES-256-ECB 的参数没写错、base64 是标准表。自己加密的夹具只能证明
// "加解密对称"，两边一起错也测不出来。
//
// 注意开头的 \uFEFF：三个镜像下发的文件**都带 UTF-8 BOM**，这里照原样保留，
// 就是为了让 cleanBase64Text 的作用被真正测到。
const nodeListFixture = "\uFEFF" +
	`X+bnzYIcwF6C7Rd3T7njPDNH08zsH9zyqCrrjCr7qcnHb1LsmIZGIHtrNVR/GiraHE6OuhvrxEzwciVvhdU0I9OYcmWTxF1K7fLfcwkn7kMQg2DZ2qpE7dKGkqKCmQijaSUOswxL1/p9pSVe/vRYEzbB5pfcAB6Yz/zVVIendBJK629QiqQndRXM9bijtZuYJtKw3YBAA26a+fy06dNszfw9v/4R8akVaSTWLOJc0nJy+9vm2t2W997vcqFL91iklKuKVEZHTtdpaLTgWExXaLjtIz2zlVZfy3jYrzKZ7x+LL7o02c6WB4HV69s1VqCJYl+3l3RNwDjJ0iRNnG9p/caZL/y6sT8i78Wc38WZhOAxkDsOFiGNpvS3eojKA0wGhpTtWsuUrXgSZ7ilEPttJvOhrJGGRJJ7Ux4HgzDYO1A=`

func TestDecodeNodeListRealCapture(t *testing.T) {
	list, err := DecodeNodeList(nodeListFixture)
	if err != nil {
		t.Fatalf("解密线路清单失败: %v", err)
	}

	hosts := list.Hosts()
	if len(hosts) == 0 {
		t.Fatal("清单里没有解析出任何线路")
	}

	// Hosts() 必须补全协议头：清单里是裸域名（www.cdngwc.cc）。
	for _, h := range hosts {
		if !strings.HasPrefix(h, "https://") {
			t.Errorf("线路 %q 没有归一化成 https:// 形式", h)
		}
	}

	for _, w := range []string{"https://www.cdngwc.cc", "https://www.cdnhjk.net"} {
		if !containsStr(hosts, w) {
			t.Errorf("清单里缺少 %q（实际: %v）", w, hosts)
		}
	}

	seen := make(map[string]bool)
	for _, h := range hosts {
		if seen[h] {
			t.Errorf("线路 %q 重复出现", h)
		}
		seen[h] = true
	}
}

// TestDecodeNodeListToleratesBOMAndWhitespace 确认清理逻辑真的生效。
//
// 这三项噪声都是实测存在的，不是臆想：
//   - 镜像下发的文件带 UTF-8 BOM
//   - 有些 CDN 会在结尾补换行
//   - 个别场景会带首尾空格
//
// Go 的 base64 是严格模式，任意一项都能让整条链路挂掉。
func TestDecodeNodeListToleratesBOMAndWhitespace(t *testing.T) {
	plain := nodeListFixture[len("\ufeff"):] // 去掉夹具里的 BOM，当"干净输入"

	variants := map[string]string{
		"原始（带 BOM）":     "\ufeff" + plain,
		"无 BOM":         plain,
		"结尾有换行":         plain + "\n",
		"首尾有空格":         "  " + plain + "  ",
		"BOM + 换行 + 空格": "\ufeff" + plain + "\r\n  ",
	}
	for name, in := range variants {
		list, err := DecodeNodeList(in)
		if err != nil {
			t.Errorf("%s: 解密失败: %v", name, err)
			continue
		}
		if len(list.Hosts()) == 0 {
			t.Errorf("%s: 没解出线路", name)
		}
	}
}

func TestDecodeNodeListRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"不是 base64":    "!!!not base64!!!",
		"base64 但长度不对": "YWJj",                     // 3 字节，不是 16 的整数倍
		"长度对但解不开":      "AAAAAAAAAAAAAAAAAAAAAA==", // 16 字节，填充必然非法
		"空串":           "",
	}
	for name, in := range cases {
		if _, err := DecodeNodeList(in); err == nil {
			t.Errorf("%s: 应当报错，但成功解析了", name)
		}
	}
}

// TestNodeListHostsOrderAndDedup 用构造的数据确认 Hosts() 的行为：
// 取 Setting/Server/JM3Server 的并集、保持出现顺序、补协议头、去重。
func TestNodeListHostsOrderAndDedup(t *testing.T) {
	list := &NodeList{
		Setting:   []string{"a.example", "b.example"},
		Server:    []string{"b.example", "c.example"},
		JM3Server: []JM3ServerEntry{"c.example", "d.example"},
	}

	got := list.Hosts()
	want := []string{
		"https://a.example",
		"https://b.example",
		"https://c.example",
		"https://d.example",
	}
	if len(got) != len(want) {
		t.Fatalf("线路数 = %d (%v), 期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %q, 期望 %q（全量: %v）", i, got[i], want[i], got)
		}
	}
}

// TestNodeListHostsNormalizesScheme 确认已经带协议头/尾斜杠的写法不会拼坏。
func TestNodeListHostsNormalizesScheme(t *testing.T) {
	list := &NodeList{Server: []string{"https://x.example/", "http://y.example"}}
	got := list.Hosts()
	want := []string{"https://x.example", "http://y.example"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %q, 期望 %q", i, got[i], want[i])
		}
	}
}

// TestJM3ServerEntryToleratesShapeChanges 确认 jm3_Server 的项能容忍结构变化。
//
// 真实清单里是 ["域名","線路名"]，但服务端加字段（变成三元组）或改成裸字符串
// 都不该让整份清单解析失败 —— 我们只关心第一项（域名）。
func TestJM3ServerEntryToleratesShapeChanges(t *testing.T) {
	cases := map[string]string{
		`["www.a.example","線路1"]`:     "www.a.example",
		`["www.b.example","線路2","x"]`: "www.b.example", // 多一项也要能解
		`"www.c.example"`:             "www.c.example", // 裸字符串
	}
	for raw, want := range cases {
		var list NodeList
		if err := json.Unmarshal([]byte(`{"jm3_Server":[`+raw+`]}`), &list); err != nil {
			t.Errorf("%s: 解析失败: %v", raw, err)
			continue
		}
		got := list.Hosts()
		if len(got) != 1 || got[0] != "https://"+want {
			t.Errorf("%s: Hosts() = %v, 期望 [https://%s]", raw, got, want)
		}
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
