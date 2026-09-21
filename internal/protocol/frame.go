// Package protocol 实现驱动本进程的 JSON 行协议（JSON Lines）。
//
// 本协议的帧格式与 heaven/bridge 的 CoreBridge 严格对齐，设计上受它的两处判定约束：
//
//  1. CoreBridge 用「消息里有没有顶层 task_id」来区分响应和事件：
//     'task_id' in msg → 当成响应，去 _pending 里找对应 promise。
//     因此**进度/事件帧绝不能带顶层 task_id**，否则会被误判成响应、提前 resolve 掉
//     宿主那个还没跑完的 promise，并且真正的结果帧到达时已经没有 pending 了。
//     事件帧把 task_id 放在 data 里传递。
//  2. CoreBridge 对错误执行 new Error(msg.error)，所以 error 字段必须是**字符串**。
//     机器可读的错误码另放在 error_code 字段里，宿主可自行取用。
//
// 帧格式：
//
//	{"task_id":1,"result":{...}}                              // 成功
//	{"task_id":1,"error":"network: ...","error_code":"network"} // 失败
//	{"event":"progress","data":{"task_id":1,"stage":"download","done":3,"total":42}}
//	{"event":"ready","data":{"pid":123,"commands":[...]}}
//	{"event":"protocol-error","data":{"line":"...","message":"..."}}
//
// 约定：
//   - stdin 读请求、stdout 写响应，一行一个完整 JSON 对象。
//   - stdout 只允许出现本包定义的帧。日志、调试一律走 stderr，
//     否则会污染协议流，宿主侧解析会直接崩掉。
//   - 一个请求可能产生 0..N 个 progress 事件，并以恰好一个结果帧收尾。
package protocol

import (
	"encoding/json"
	"fmt"
)

// Version 是协议版本，宿主可据此判断能力集。
const Version = "1.0"

// 事件名（event 字段取值）。
const (
	// EventReady 进程就绪，宿主可开始下发请求。data 里带 pid / 命令表。
	EventReady = "ready"
	// EventProgress 任务中间进度，同一个任务可出现多次。
	EventProgress = "progress"
	// EventProtocolError 宿主发来的行无法解析（连 task_id 都拿不到），
	// 无法归到某个任务上，只能通过事件通道告知。
	EventProtocolError = "protocol-error"
)

// TaskID 兼容 JSON 数字与字符串两种写法，并原样保留调用方的写法。
//
// CoreBridge 用数字当 Map 的 key，所以必须原样回显：宿主传 1 就要回 1，
// 不能回 "1"，否则 pending 查不到、那个 promise 会一直挂到超时。
type TaskID struct {
	raw json.RawMessage
}

// NewTaskID 用任意可 JSON 序列化的值构造 TaskID（数字、字符串均可）。
func NewTaskID(v any) TaskID {
	b, err := json.Marshal(v)
	if err != nil {
		return TaskID{}
	}
	return TaskID{raw: b}
}

// TaskIDFromString 用字符串构造 TaskID，回包时同样输出字符串。
func TaskIDFromString(s string) TaskID { return NewTaskID(s) }

// UnmarshalJSON 实现 json.Unmarshaler，原样记录字面量。
func (t *TaskID) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("task_id 为空")
	}
	t.raw = append(t.raw[:0], b...)
	return nil
}

// MarshalJSON 实现 json.Marshaler。
func (t TaskID) MarshalJSON() ([]byte, error) {
	if len(t.raw) == 0 {
		return []byte("null"), nil
	}
	return t.raw, nil
}

// IsZero 报告 task_id 是否缺省（未提供或为 null）。
func (t TaskID) IsZero() bool {
	return len(t.raw) == 0 || string(t.raw) == "null"
}

// String 返回 task_id 的字面量（字符串形式会去掉引号），用于日志与去重。
func (t TaskID) String() string {
	if t.IsZero() {
		return ""
	}
	s := string(t.raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if err := json.Unmarshal(t.raw, &out); err == nil {
			return out
		}
	}
	return s
}

// Request 是宿主下发的一条请求。一行一个。
type Request struct {
	Cmd    string          `json:"cmd"`
	TaskID TaskID          `json:"task_id"`
	Params json.RawMessage `json:"params,omitempty"`

	// Raw 保留原始请求行，便于出错时把「收到的到底是什么」原样带回给宿主。
	Raw json.RawMessage `json:"-"`
}

// DecodeParams 把 params 解析到 v。params 缺省时把 v 置为零值，不报错。
func (r *Request) DecodeParams(v any) error {
	if len(r.Params) == 0 || string(r.Params) == "null" {
		return nil
	}
	if err := json.Unmarshal(r.Params, v); err != nil {
		return Wrap(CodeInvalidParams, err, "params 解析失败")
	}
	return nil
}

// Response 是结果帧（成功或失败）。
//
// Result 与 Error 互斥；Error 为字符串以适配 CoreBridge 的 new Error(msg.error)。
type Response struct {
	TaskID *TaskID `json:"task_id,omitempty"`
	Result any     `json:"result,omitempty"`
	// Error 失败时的错误文案，格式为 "错误码: 详细描述"。
	Error string `json:"error,omitempty"`
	// ErrorCode 机器可读的错误码，成功时省略。宿主应基于它分支，不要匹配 Error 文案。
	ErrorCode string `json:"error_code,omitempty"`
}

// Event 是单向事件帧，绝不能带顶层 task_id。
type Event struct {
	Event string `json:"event"`
	Data  any    `json:"data,omitempty"`
}

// ProgressData 是 event=progress 时 data 字段的结构。
type ProgressData struct {
	// TaskID 事件归属的任务，放在 data 里是为了不触发 CoreBridge 的响应判定。
	TaskID TaskID `json:"task_id"`
	// Stage 阶段名，例如 probe / download / restore / pdf / merge。
	Stage string `json:"stage"`
	// Done 当前阶段已完成的数量。
	Done int `json:"done"`
	// Total 当前阶段的总量；-1 表示总量未知。
	Total int `json:"total"`
	// Message 人类可读的一句话，可空。
	Message string `json:"message,omitempty"`
	// Extra 阶段相关的附加字段（当前项、输出路径、失败数等）。
	Extra map[string]any `json:"extra,omitempty"`
}

// taskIDPtr 只在 task_id 非缺省时返回指针，避免结果帧里出现 "task_id": null。
func taskIDPtr(id TaskID) *TaskID {
	if id.IsZero() {
		return nil
	}
	return &id
}
