package client

import (
	"fmt"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
)

// APIError 表示服务端返回的业务异常（HTTP 200 但 code != 200）。
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("服务端业务错误: code=%d, message=%s", e.Code, e.Message)
}

// ErrorCode 实现 protocol.Coded，让宿主收到统一的错误码而不是 internal。
func (e *APIError) ErrorCode() string { return protocol.CodeAPI }

func (e *NetworkError) ErrorCode() string { return protocol.CodeNetwork }

func (e *StatusError) ErrorCode() string { return protocol.CodeNetwork }
