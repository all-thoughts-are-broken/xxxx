package utils

import (
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"testing"
)

// jsGetNum 是 APP 端 utils/Function.js 里 get_num 的**逐行直译**，只用于测试。
//
// 故意照抄原实现的所有怪癖（charCodeAt、switch 的默认值、中间区间不取模），
// 目的是把「实现」与「参照」彻底解耦：如果 CountImageSegments 为了看起来更
// 合理而偏离原算法，这里立刻会红。
func jsGetNum(aid string, page string) int {
	// JS: var num = 10; var key = md5(aid + page).substr(-1).charCodeAt();
	num := 10
	sum := md5.Sum([]byte(aid + page))
	hash := hex.EncodeToString(sum[:])
	key := int(hash[len(hash)-1]) // charCodeAt() = ASCII 码，不是十六进制值

	n, _ := strconv.Atoi(aid)
	if n >= 268850 && n <= 421925 {
		key = key % 10
	} else if n >= 421926 {
		key = key % 8
	}

	// JS: switch (key) { case 0: num = 2; ... case 9: num = 20; }
	// 关键：key 不在 0..9 时没有任何 case 命中，num 保持初始值 10。
	switch key {
	case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9:
		num = key*2 + 2
	}
	return num
}

// TestTokenMatchesRealCapture 用真实抓包核对签名算法。
//
// 这条断言的价值在于：token 与响应解密共用同一个 secret，
// 所以它一旦成立，就同时锁住了「鉴权头算法」和「AES 密钥派生」两件事。
func TestTokenMatchesRealCapture(t *testing.T) {
	const (
		ts     = "1752484996"
		ver    = "1.8.0"
		secret = "185Hcomic3PAPP7R"
		want   = "3ffb5bcfa3f94c10509b02d1aec20634"
	)

	tokenParam, token := GetTokenAndTokenParam(ts, ver, secret)

	if token != want {
		t.Errorf("token = %q, 期望 %q", token, want)
	}
	if tokenParam != ts+","+ver {
		t.Errorf("tokenparam = %q, 期望 %q", tokenParam, ts+","+ver)
	}
}

// TestCountImageSegmentsMatchesJS 对段数算法做穷举对拍。
func TestCountImageSegmentsMatchesJS(t *testing.T) {
	const scrambleID = 220980

	type tc struct {
		aid  int
		name string
	}
	var cases []tc

	// 四个区间的边界（含 ±1）
	boundaries := []int{
		scrambleID - 1, scrambleID, scrambleID + 1,
		268849, 268850, 268851,
		421924, 421925, 421926, 421927,
		1, 1000, 100000, 999999, 3000000,
	}
	for _, aid := range boundaries {
		for _, name := range []string{"00001", "00011", "00042", "xyz", "99999"} {
			cases = append(cases, tc{aid: aid, name: name})
		}
	}

	// 区间内部也要一致
	for aid := 220975; aid < 221000; aid++ {
		cases = append(cases, tc{aid: aid, name: "00001"})
	}
	for aid := 268840; aid < 268860; aid++ {
		cases = append(cases, tc{aid: aid, name: "00007"})
	}
	for aid := 421915; aid < 421940; aid++ {
		cases = append(cases, tc{aid: aid, name: "00003"})
	}

	for _, c := range cases {
		got := CountImageSegments(scrambleID, c.aid, c.name)

		// 参照实现：aid < scramble_id 时 JS 走早退分支（不还原），段数记 0。
		want := 0
		if c.aid >= scrambleID {
			want = jsGetNum(strconv.Itoa(c.aid), c.name)
		}

		if got != want {
			t.Errorf("CountImageSegments(%d, %d, %q) = %d, JS 参照 = %d",
				scrambleID, c.aid, c.name, got, want)
		}
	}
}

// TestCountImageSegmentsAlwaysTenInLowerRange 专门锁住那个最容易被"优化"掉的区间。
//
// aid ∈ [scramble_id, 268850) 时段数恒为 10，与文件名无关。
func TestCountImageSegmentsAlwaysTenInLowerRange(t *testing.T) {
	for _, aid := range []int{220980, 230000, 260000, 268849} {
		for _, name := range []string{"00001", "00002", "abcd", "zzzz", "12345"} {
			if got := CountImageSegments(DefaultScrambleID, aid, name); got != 10 {
				t.Errorf("aid=%d name=%q 段数 = %d, 期望恒为 10", aid, name, got)
			}
		}
	}
}

// TestCountImageSegmentsDelegatesToExpression 确认中高区间确实按 md5 末字符取模。
//
// 如果哪天有人把两个区间的取模基数写反（%10 与 %8），这条会立刻失败。
func TestCountImageSegmentsDelegatesToExpression(t *testing.T) {
	// 中区间：268850 <= aid <= 421925
	for _, aid := range []int{268850, 300000, 399054, 421925} {
		mid := CountImageSegments(DefaultScrambleID, aid, "00011")
		if mid == 10 {
			// 恰好算到 10 是可能的，不构成问题；这里只要求与参照一致。
		}
		if want := jsGetNum(strconv.Itoa(aid), "00011"); mid != want {
			t.Errorf("中区间 aid=%d: %d != %d", aid, mid, want)
		}
	}
	// 高区间：aid >= 421926
	for _, aid := range []int{421926, 500000, 3000000} {
		high := CountImageSegments(DefaultScrambleID, aid, "00011")
		if want := jsGetNum(strconv.Itoa(aid), "00011"); high != want {
			t.Errorf("高区间 aid=%d: %d != %d", aid, high, want)
		}
	}
}

// TestCountImageSegmentsRange 校验段数的取值域，防止将来引入奇数或越界值。
func TestCountImageSegmentsRange(t *testing.T) {
	for aid := 200000; aid < 500000; aid += 997 {
		seg := CountImageSegments(DefaultScrambleID, aid, "00001")
		if seg == 0 {
			continue
		}
		if seg < 2 || seg > 20 {
			t.Fatalf("aid=%d 段数 %d 超出 [2,20]", aid, seg)
		}
		if seg%2 != 0 {
			t.Fatalf("aid=%d 段数 %d 是奇数（原算法只产生偶数）", aid, seg)
		}
	}
}

// TestImageBaseName 覆盖路径 / URL / 裸名三种输入形式。
func TestImageBaseName(t *testing.T) {
	cases := map[string]string{
		"00011":                           "00011",
		"00011.webp":                      "00011",
		"/media/photos/399054/00011.webp": "00011",
		"/media/photos/399054/00011.webp?t=1783091202": "00011",
		`D:\download\399054\00011.webp`:                "00011",
		"https://cdn.xxx.cc/media/photos/1/00042.webp": "00042",
		"  00007.webp  ": "00007",
	}
	for in, want := range cases {
		if got := imageBaseName(in); got != want {
			t.Errorf("imageBaseName(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestImageBaseNameAffectsSegments 确认「同名的不同路径形式」算出的段数一致。
//
// 这是下载链路里最容易出错的点：原始文件落盘后按本地路径算段数，
// 而 APP 端是拿 URL 里的文件名算。两条路径必须等价，否则同一张图
// 在"下载时"和"还原时"会算出不同的段数。
func TestImageBaseNameAffectsSegments(t *testing.T) {
	const aid = 399054
	forms := []string{
		"00011",
		"00011.webp",
		"/media/photos/399054/00011.webp",
		"/media/photos/399054/00011.webp?t=1783091202",
		`G:\download\399054\00011.webp`,
	}
	base := CountImageSegments(DefaultScrambleID, aid, forms[0])
	for _, f := range forms[1:] {
		if got := CountImageSegments(DefaultScrambleID, aid, f); got != base {
			t.Errorf("形式 %q 得到段数 %d, 期望与 %q 一致 (%d)", f, got, forms[0], base)
		}
	}
}

// TestDecodeRespDataRejectsGarbage 确认解密对垃圾输入给出明确错误而不是 panic。
func TestDecodeRespDataRejectsGarbage(t *testing.T) {
	cases := []string{
		"",              // 空
		"not-base64!!!", // 非法 base64
		"YWJj",          // 合法 base64 但长度不是 16 的整数倍
	}
	for _, in := range cases {
		if _, err := DecodeRespData(in, "1752484996", "185Hcomic3PAPP7R"); err == nil {
			t.Errorf("DecodeRespData(%q) 期望报错，实际成功", in)
		}
	}
}

// TestDecodeRespDataRoundTrip 自造密文，确认密钥派生与填充处理闭合。
//
// 测试内反向实现 AES-ECB 加密（标准库不提供 ECB），生成密文交给被测的解密函数 ——
// 与「用真实抓包核对」互补：前者锁算法结构，后者锁参数取值。
func TestDecodeRespDataRoundTrip(t *testing.T) {
	const (
		ts     = "1752484996"
		secret = "185Hcomic3PAPP7R"
		plain  = `{"code":200,"data":{"hello":"世界"}}`
	)

	encrypted := aesECBEncryptBase64(t, plain, md5Hex(ts+secret))

	got, err := DecodeRespData(encrypted, ts, secret)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if got != plain {
		t.Errorf("解密结果 = %q, 期望 %q", got, plain)
	}

	// 换个时间戳必须解不开（密钥随时间戳变化）。
	// 注意：ECB 没有完整性校验，所以「解不开」的表现通常是去填充失败，
	// 极小概率会碰巧解出无意义内容 —— 因此这里只断言"得不到原文"。
	if alt, err := DecodeRespData(encrypted, "1752484997", secret); err == nil && alt == plain {
		t.Errorf("用错误时间戳竟然解出了原文，密钥派生没有依赖时间戳")
	}
}

// TestPkcs7Unpad 覆盖填充校验的边界。
func TestPkcs7Unpad(t *testing.T) {
	valid := map[string][]byte{
		"整块填充":      bytesRepeat(0x10, 16),
		"末尾 1 字节填充": append([]byte{0xAA, 0xBB, 0xCC}, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x01),
	}
	for name, in := range valid {
		if _, err := pkcs7Unpad(in); err != nil {
			t.Errorf("%s: 期望合法，实际报错 %v", name, err)
		}
	}

	invalid := map[string][]byte{
		"空":           {},
		"padding 为 0": {0xAA, 0x00},
		"padding 超块":  {0xAA, 0x11},
		"填充字节不一致":     {0xAA, 0xBB, 0xCC, 0x03, 0x03, 0x02},
	}
	for name, in := range invalid {
		if _, err := pkcs7Unpad(in); err == nil {
			t.Errorf("%s: 期望报错，实际成功", name)
		}
	}
}

// ---- 测试辅助 ----

// aesECBEncryptBase64 是 DecodeRespData 的逆运算，仅用于构造测试密文。
func aesECBEncryptBase64(t *testing.T, plain, key string) string {
	t.Helper()

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("构造 AES: %v", err)
	}

	data := pkcs7Pad([]byte(plain), block.BlockSize())
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += block.BlockSize() {
		block.Encrypt(out[i:i+block.BlockSize()], data[i:i+block.BlockSize()])
	}
	return base64.StdEncoding.EncodeToString(out)
}

// pkcs7Pad 补 PKCS#7 填充。
func pkcs7Pad(data []byte, blockSize int) []byte {
	n := blockSize - len(data)%blockSize
	return append(data, bytesRepeat(byte(n), n)...)
}

// bytesRepeat 返回 n 个字节 b，等价于 bytes.Repeat([]byte{b}, n)。
func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
