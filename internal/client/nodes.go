package client

import (
	"context"
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 本文件负责「动态获取 API 线路清单」。
//
// 为什么不能靠硬编码列表：官方会把线路清单做成一个加密的 newsvr-2025.txt，
// 放在几个不同的对象存储节点上（三个镜像，地域不同），APP/网页每次启动都拉一份，
// 于是线路轮换不需要发新版本。我们如果只硬编码一份，线路一换就全盘失效，
// 而失效的表现还很隐蔽 —— 旧域名可能仍然 HTTP 200 且信封结构合法，
// 只是响应解不开（看起来像"加解密算法坏了"）。
//
// 清单本身也是加密的，规则与接口响应的加解密**不是同一套**：
// key = md5(清单盐) 的十六进制字符串按 UTF-8 取 32 字节，AES-256-ECB，PKCS#7。

// NodeListSecret 线路清单的加密盐。
//
// 不要把它和 config.DefaultSecret 弄混：那个用于接口签名 + 响应解密，
// 这个只用于解开线路清单文件。
const NodeListSecret = "diosfjckwpqpdfjkvnqQjsik"

// NodeListMirrors 线路清单的镜像地址，按"实测延迟从低到高"排列。
//
// 三份内容等价，任一可用即可。2026-09-21 实测（本机）：
// 北京 0.34s、新加坡 0.57s、香港 1.78s。
var NodeListMirrors = []string{
	"https://rup4a04-c03.tos-cn-beijing.bytepluses.com.cn/newsvr-2025.txt",
	"https://rup4a04-c01.tos-ap-southeast-1.bytepluses.com/newsvr-2025.txt",
	"https://rup4a04-c02.tos-cn-hongkong.bytepluses.com/newsvr-2025.txt",
}

// NodeList 是线路清单解析后的结构。
//
// 三个字段是历史演进留下的：Setting 给网页端用，Server 是通用列表，
// JM3Server 带线路名（"線路1"…）给 APP 展示用。三者的域名集合可能不完全一致
// （例如 JM3Server 会多一条备用线路），所以取并集。
type NodeList struct {
	Setting   []string         `json:"Setting"`
	Server    []string         `json:"Server"`
	JM3Server []JM3ServerEntry `json:"jm3_Server"`
}

// JM3ServerEntry 是 jm3_Server 里的一项，形如 ["www.cdnhjk.net", "線路1"]。
//
// 类型定义成 string 而不是 [2]string，是为了容忍数组长度变化：
// 直接写 [2]string 的话，服务端哪天给个三元组就会让整份清单解析失败，
// 而这里我们只关心第一项（域名）。
type JM3ServerEntry string

// UnmarshalJSON 容忍 ["域名","线路名"] 与 直接给字符串 两种写法。
func (e *JM3ServerEntry) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		if len(arr) > 0 {
			*e = JM3ServerEntry(arr[0])
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("jm3_Server 项既不是字符串也不是字符串数组: %s", string(b))
	}
	*e = JM3ServerEntry(s)
	return nil
}

// Hosts 返回清单里全部 API 域名，转成 https:// 形式并去重（保持出现顺序）。
//
// 顺序有意义：先 Setting/Server，再 JM3Server 独有的；去重保留首次出现的位置，
// 这样即使后面没做延迟测速，也能按官方给出的优先级用。
func (n *NodeList) Hosts() []string {
	if n == nil {
		return nil
	}

	var out []string
	seen := make(map[string]bool)
	add := func(raw string) {
		h := normalizeHost(raw)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}

	for _, h := range n.Setting {
		add(h)
	}
	for _, h := range n.Server {
		add(h)
	}
	for _, e := range n.JM3Server {
		add(string(e))
	}
	return out
}

// normalizeHost 把清单里的裸域名（www.cdngwc.cc）转成 https://www.cdngwc.cc。
func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return strings.TrimRight(raw, "/")
	}
	return "https://" + strings.TrimRight(raw, "/")
}

// DecodeNodeList 解密并解析线路清单。
//
// 清单里域名是裸的（没有协议头），解析后由 Hosts() 补全。
func DecodeNodeList(encrypted string) (*NodeList, error) {
	key := md5HexOf(NodeListSecret)
	ciphertext, err := base64.StdEncoding.DecodeString(cleanBase64Text(encrypted))
	if err != nil {
		return nil, fmt.Errorf("清单 base64 解码失败: %w", err)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("清单密文长度 %d 不是 AES 块大小 %d 的整数倍",
			len(ciphertext), aes.BlockSize)
	}

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("构造 AES 密码块失败: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(plain[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}

	plain, err = pkcs7Unpad(plain)
	if err != nil {
		return nil, fmt.Errorf("清单解密后去填充失败: %w", err)
	}

	var list NodeList
	if err := json.Unmarshal(plain, &list); err != nil {
		return nil, fmt.Errorf("清单不是合法 JSON: %w", err)
	}
	if len(list.Hosts()) == 0 {
		return nil, fmt.Errorf("清单里没有任何线路")
	}
	return &list, nil
}

// cleanBase64Text 清掉 base64 文本里的噪声，让它能过 Go 的严格解码。
//
// 两项噪声都是实测存在的，不是防御性编程：
//   - **UTF-8 BOM**：三个镜像下发的文件都以 EF BB BF 开头。Go 的
//     base64.StdEncoding 是严格模式，不剥掉会直接报
//     "illegal base64 data at input byte 0"。用 strings.TrimSpace 是剥不掉的，
//     U+FEFF 不在 unicode.IsSpace 里。
//   - 换行/空白：有些 CDN 会给 .txt 加上结尾换行。
//
// 顺带把 base64 字母表以外的字符丢掉，这样即便将来文件里混进注释也不至于硬失败；
// 真正的区分度来自 AES 解密本身（能解开才算数）。
func cleanBase64Text(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == '/' || r == '=':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// md5HexOf 返回 s 的 md5 十六进制串（32 字符）。
func md5HexOf(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pkcs7Unpad 去掉 PKCS#7 填充。
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("数据为空")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > aes.BlockSize || n > len(data) {
		return nil, fmt.Errorf("非法的填充长度 %d", n)
	}
	for i := len(data) - n; i < len(data); i++ {
		if data[i] != byte(n) {
			return nil, fmt.Errorf("填充字节不一致")
		}
	}
	return data[:len(data)-n], nil
}

// FetchNodeList 依次尝试镜像拉取并解密线路清单。
//
// 任一镜像成功即返回。全部失败时返回最后一个错误，并由调用方回退到
// config.DefaultBaseURLs —— 拉不到清单不该让整个工具不可用。
//
// timeout <= 0 时用 15 秒。镜像之间是串行的（正常情况第一个就成），
// 单个镜像失败不会拖垮整体。
func FetchNodeList(ctx context.Context, proxy string, mirrors []string, timeout time.Duration) (*NodeList, error) {
	if len(mirrors) == 0 {
		mirrors = NodeListMirrors
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	hc := plainHTTPClient(proxy, timeout)

	var lastErr error
	for _, mirror := range mirrors {
		mirror = strings.TrimSpace(mirror)
		if mirror == "" {
			continue
		}
		list, err := fetchNodeListFrom(ctx, hc, mirror)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", mirror, err)
			continue
		}
		return list, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的清单镜像")
	}
	return nil, lastErr
}

func fetchNodeListFrom(ctx context.Context, hc *http.Client, mirror string) (*NodeList, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mirror, nil)
	if err != nil {
		return nil, err
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return DecodeNodeList(string(body))
}

// plainHTTPClient 建一个**不带签名头**的客户端。
//
// 线路清单放在第三方对象存储上，没必要也不需要把 token 头发过去 ——
// 签名头是给 API 线路用的，发到别处纯属多余的信息泄露。
func plainHTTPClient(proxy string, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}
