package client

import (
	"io"
	"strings"
)

// readLimited 读取至多 n 字节（用于日志/错误信息，或已知不会太大的响应体）。
//
// 返回 []byte 而不是 string：调用方多数要直接喂给 json.Unmarshal，
// 提前转 string 只会白白多一次整块拷贝（响应体上限是 32 MiB 级）。
func readLimited(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

// preview 截断长文本用于错误信息，避免把整个响应体塞进日志。
func preview(b []byte) string {
	const n = 200
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
