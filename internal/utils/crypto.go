package utils

import (
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// DefaultScrambleID 是切图算法的默认阈值：aid >= 该值的作品，服务端返回的图片是
// 被竖向切段打乱过的；aid < 220980 的老作品图片本身是完整的。
const DefaultScrambleID = 220980

// md5Hex 返回字符串的 MD5 十六进制表示。
func md5Hex(key string) string {
	sum := md5.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}

// GetTokenAndTokenParam 计算 APP 请求所需的鉴权头。
//
// 返回值顺序为 (tokenparam, token)，与需要写入的 HTTP 头一一对应：
//
//	tokenparam: "1752484996,1.8.0"          时间戳,APP版本
//	token:      md5(时间戳 + secret)         32 位小写十六进制
//
// 注意时间戳和响应解密共用 secret（已用真实抓包核对：md5("1752484996"+"185Hcomic3PAPP7R")
// == 抓包里的 token）。所以 secret 一旦写错，鉴权和解密会同时失败。
func GetTokenAndTokenParam(timestamp, version, secret string) (tokenparam string, token string) {
	tokenparam = timestamp + "," + version
	token = md5Hex(timestamp + secret)
	return tokenparam, token
}

// DecodeRespData 解密接口返回的密文。
//
// 算法与 APP 端一致：key = md5(timestamp + secret) 作为 32 字节 AES-256 密钥，
// 密文先 Base64 解码，再按 AES-256-ECB 分块解密，最后去掉 PKCS#7 填充。
func DecodeRespData(encryptedData, timestamp, secret string) (string, error) {
	key := md5Hex(timestamp + secret)

	dataB64, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encryptedData))
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(dataB64) == 0 {
		return "", fmt.Errorf("密文为空")
	}
	if len(dataB64)%aes.BlockSize != 0 {
		return "", fmt.Errorf("密文长度 %d 不是 AES 块大小 %d 的整数倍", len(dataB64), aes.BlockSize)
	}

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", fmt.Errorf("构造 AES 密码块失败: %w", err)
	}

	// ECB 模式需要手动分块解密（标准库不提供 ECB）。
	decrypted := make([]byte, len(dataB64))
	for i := 0; i < len(dataB64); i += aes.BlockSize {
		block.Decrypt(decrypted[i:i+aes.BlockSize], dataB64[i:i+aes.BlockSize])
	}

	decrypted, err = pkcs7Unpad(decrypted)
	if err != nil {
		return "", fmt.Errorf("去除填充失败: %w", err)
	}
	return string(decrypted), nil
}

// pkcs7Unpad 去除 PKCS#7 填充。
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("数据为空")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > aes.BlockSize {
		return nil, fmt.Errorf("非法的填充长度 %d", padding)
	}
	for i := len(data) - padding; i < len(data); i++ {
		if data[i] != byte(padding) {
			return nil, fmt.Errorf("填充字节不一致")
		}
	}
	return data[:len(data)-padding], nil
}

// CountImageSegments 计算某张图被切成了多少段，返回 0 表示图片未被切分、无需还原。
//
// scrambleID 取自阅读页响应的 scramble_id 字段；传 0 时回退到 DefaultScrambleID。
// nameOrPath 可以是磁盘路径，也可以是 URL 或裸文件名——算法里参与 MD5 的只有
// 去掉扩展名后的文件名（例如 00011.webp → "00011"），所以三种形式结果一致。
//
// 算法（与 APP 端 get_num 逐行等价）：
//
//	aid < scramble_id            → 0（老作品不切图）
//	scramble_id <= aid < 268850  → 10（恒定！见下方说明）
//	268850 <= aid <= 421925      → (md5(aid+name) 末字符 ASCII) % 10 * 2 + 2
//	aid >= 421926                → (md5(aid+name) 末字符 ASCII) % 8  * 2 + 2
//
// 即段数永远是 2..20 之间的偶数。
//
// ⚠️ 中间那段「恒定 10」不是简化，是原算法的真实行为，很容易看漏：
// JS 里 key 取的是末位字符的 charCodeAt()（十六进制字符的 ASCII，48..57 或
// 97..102），只有落在后两个区间才会被 %10 / %8 归一化到 0..9。而不归一化时
// 48..102 这些值一个都命中不了 switch 的 case 0..9，于是 num 保持初始值 10。
// 早期实现把这里写成了「%10*2+2」，会在这个区间算出 4/6/8… 之类的段数，
// 还原出来是错位的——已用逐行直译的参照实现锁定（见 crypto_test.go）。
func CountImageSegments(scrambleID, aid int, nameOrPath string) int {
	if scrambleID <= 0 {
		scrambleID = DefaultScrambleID
	}
	if aid < scrambleID {
		return 0
	}
	// 关键：这一段恒为 10，不参与 MD5。
	if aid < 268850 {
		return 10
	}

	x := 8
	if aid < 421926 {
		x = 10
	}

	hash := md5Hex(fmt.Sprintf("%d%s", aid, imageBaseName(nameOrPath)))
	last := int(hash[len(hash)-1]) // 末位十六进制字符的 ASCII 码
	return last%x*2 + 2
}

// imageBaseName 从路径 / URL / 裸文件名里取出「不含扩展名的文件名」。
//
// 需要同时兼容：
//
//	/media/photos/399054/00011.webp
//	/media/photos/399054/00011.webp?t=1783091202   （阅读页返回的签名 URL）
//	D:\download\399054\00011.webp
//	00011
func imageBaseName(nameOrPath string) string {
	s := strings.TrimSpace(nameOrPath)
	// 去掉 query / fragment（URL 里的签名参数会污染扩展名）
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	// URL 统一成分隔符，filepath.Base 在 Windows 上不认 '/'
	if strings.Contains(s, "/") {
		s = s[strings.LastIndex(s, "/")+1:]
	}
	if u, err := url.Parse(s); err == nil && u.Path != "" {
		s = u.Path
	}
	s = filepath.Base(s)
	if ext := filepath.Ext(s); ext != "" {
		s = strings.TrimSuffix(s, ext)
	}
	return s
}
