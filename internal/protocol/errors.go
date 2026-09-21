package protocol

import (
	"context"
	"errors"
	"fmt"
)

// 错误码。宿主应基于 code 分支，不要匹配 message 文案。
const (
	// CodeBadRequest 请求行本身不是合法 JSON，或缺少 cmd。
	CodeBadRequest = "bad_request"
	// CodeUnknownCommand 未知命令。
	CodeUnknownCommand = "unknown_command"
	// CodeInvalidParams params 解析失败或取值非法。
	CodeInvalidParams = "invalid_params"
	// CodeNotFound 目标不存在（作品不存在、文件不存在等）。
	CodeNotFound = "not_found"
	// CodeNetwork 网络层失败：DNS、连接、TLS、超时等。
	CodeNetwork = "network"
	// CodeAPI 服务端返回了业务错误码。
	CodeAPI = "api"
	// CodeDecrypt 响应解密失败（通常是时间戳/密钥不匹配或线路返回了非预期内容）。
	CodeDecrypt = "decrypt"
	// CodeIO 本地文件读写失败。
	CodeIO = "io"
	// CodeCanceled 任务被宿主取消。
	CodeCanceled = "canceled"
	// CodeTimeout 任务超时。
	CodeTimeout = "timeout"
	// CodePanic handler 内部 panic，已被协议层捕获。
	CodePanic = "panic"
	// CodeInternal 其它未分类的内部错误。
	CodeInternal = "internal"
)

// CodedError 是带机器可读错误码的错误。
type CodedError struct {
	Code    string
	Message string
	Cause   error
}

// Error 实现 error。
func (e *CodedError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	if e.Message == "" {
		return e.Cause.Error()
	}
	return e.Message + ": " + e.Cause.Error()
}

// Unwrap 支持 errors.Is / errors.As 逐层下钻。
func (e *CodedError) Unwrap() error { return e.Cause }

// Errorf 构造一个带错误码的错误。
func Errorf(code, format string, args ...any) error {
	return &CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap 在 err 外面套一层错误码与上下文描述。err 为 nil 时返回 nil。
func Wrap(code string, err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	msg := ""
	if len(args) > 0 || format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return &CodedError{Code: code, Message: msg, Cause: err}
}

// Coded 让其它包的错误类型也能自带错误码，而不必把 protocol 包嵌进自己的错误链。
//
// 只要实现了 ErrorCode() string，CodeOf 就能识别——client 包的 NetworkError /
// StatusError / APIError 就是这么做的。
type Coded interface {
	ErrorCode() string
}

// CodeOf 返回错误码。未标注错误码的错误会按 ctx 错误做一次归类，其余归为 internal。
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var ce *CodedError
	if errors.As(err, &ce) && ce.Code != "" {
		return ce.Code
	}
	var coded Coded
	if errors.As(err, &coded) {
		if code := coded.ErrorCode(); code != "" {
			return code
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout
	default:
		return CodeInternal
	}
}

// MessageOf 返回给宿主看的错误文案，尽量展开完整链路。
func MessageOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
