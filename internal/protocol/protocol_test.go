package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
)

// runServer 喂入若干请求行，返回逐行解析后的事件/响应帧。
func runServer(t *testing.T, lines []string, register func(*Server)) []map[string]any {
	t.Helper()

	var out bytes.Buffer
	// 日志接到 discard：handler panic 时协议层会把完整堆栈写进日志，
	// 那是预期行为，但会把测试输出淹掉。
	srv := NewServer(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out,
		WithLogger(log.New(io.Discard, "", 0)))
	if register != nil {
		register(srv)
	}

	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}

	var frames []map[string]any
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("输出不是合法 JSON 行: %q (%v)", line, err)
		}
		frames = append(frames, m)
	}
	return frames
}

// TestProgressEventHasNoTopLevelTaskID 是本包最重要的不变量。
//
// CoreBridge 的判定逻辑是 `if ('task_id' in msg) → 当成响应`。一旦进度帧带上
// 顶层 task_id，宿主会把它误认为结果、提前 resolve 掉那个 promise，
// 而真正的结果帧到达时已经没有 pending 可查 —— 表现为"调用莫名其妙提前返回"。
func TestProgressEventHasNoTopLevelTaskID(t *testing.T) {
	frames := runServer(t, []string{
		`{"cmd":"work","task_id":7}`,
	}, func(s *Server) {
		s.Handle("work", func(ctx context.Context, task *Task) (any, error) {
			task.Progress("download", 1, 3, "第一张", map[string]any{"file": "00001.webp"})
			task.Progressf("download", 2, 3, "第 %d 张", 2)
			return map[string]any{"ok": true}, nil
		})
	})

	var progresses []map[string]any
	var results []map[string]any
	for _, f := range frames {
		switch {
		case f["event"] == "progress":
			progresses = append(progresses, f)
		case f["task_id"] != nil:
			results = append(results, f)
		}
	}

	if len(progresses) != 2 {
		t.Fatalf("期望 2 条进度事件，实际 %d 条（帧: %v）", len(progresses), frames)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果帧，实际 %d 条", len(results))
	}

	for _, p := range progresses {
		if _, bad := p["task_id"]; bad {
			t.Fatalf("进度帧带了顶层 task_id，会触发宿主误判为响应: %v", p)
		}
		// 但 data 里必须带 task_id，否则宿主无法把进度归属到任务上。
		data, ok := p["data"].(map[string]any)
		if !ok {
			t.Fatalf("进度帧缺少 data: %v", p)
		}
		if data["task_id"] == nil {
			t.Fatalf("进度帧的 data 里缺少 task_id: %v", p)
		}
	}

	// 结果帧必须有 task_id，且原样回显数字 7（不是字符串 "7"）。
	if got := results[0]["task_id"]; got != float64(7) {
		t.Errorf("结果帧 task_id = %#v, 期望数字 7", got)
	}
}

// TestTaskIDPreservesForm 确认 task_id 原样回显。
//
// 宿主侧用数字当 Map key。宿主传 1 就必须回 1，回 "1" 会让 pending 查不到。
//
// 注意每个 task_id 单独发一次请求：请求是并发执行的，响应到达顺序**没有任何保证**
// （这也是协议的设计意图 —— 宿主必须靠 task_id 匹配，不能靠顺序）。
func TestTaskIDPreservesForm(t *testing.T) {
	cases := []struct {
		line string
		want any
	}{
		{`{"cmd":"echo","task_id":123}`, float64(123)},
		{`{"cmd":"echo","task_id":"abc-1"}`, "abc-1"},
		{`{"cmd":"echo","task_id":0}`, float64(0)},
	}

	for _, c := range cases {
		frames := runServer(t, []string{c.line}, func(s *Server) {
			s.Handle("echo", func(ctx context.Context, task *Task) (any, error) {
				return "ok", nil
			})
		})

		var got any
		var seen bool
		for _, f := range frames {
			if v, ok := f["task_id"]; ok {
				got, seen = v, true
			}
		}
		if !seen {
			t.Fatalf("输入 %s 没有拿到带 task_id 的响应: %v", c.line, frames)
		}
		if got != c.want {
			t.Errorf("输入 %s 的 task_id 回显 = %#v, 期望 %#v", c.line, got, c.want)
		}
	}
}

// TestResponsesAreNotOrdered 把「响应顺序无保证」这件事固化成一条断言。
//
// 它不是缺陷而是设计：宿主必须靠 task_id 匹配响应。写这条测试是为了
// 防止有人以后误以为可以靠顺序配对，从而引入隐蔽的错配 bug。
func TestResponsesAreNotOrdered(t *testing.T) {
	// 前一个任务故意慢，后一个快 —— 若实现变成了顺序执行，这里会失败。
	frames := runServer(t, []string{
		`{"cmd":"slow","task_id":1}`,
		`{"cmd":"fast","task_id":2}`,
	}, func(s *Server) {
		gate := make(chan struct{})
		s.Handle("slow", func(ctx context.Context, task *Task) (any, error) {
			<-gate
			return "slow", nil
		})
		s.Handle("fast", func(ctx context.Context, task *Task) (any, error) {
			close(gate) // 放行 slow
			return "fast", nil
		})
	})

	// 两条响应都必须存在（顺序不作断言，只确认并发确实发生了）
	var ids []any
	for _, f := range frames {
		if f["task_id"] != nil {
			ids = append(ids, f["task_id"])
		}
	}
	if len(ids) != 2 {
		t.Fatalf("期望 2 条响应，实际 %d: %v", len(ids), frames)
	}
}

// TestErrorFrameShape 确认错误帧的形状符合 CoreBridge 的 new Error(msg.error)。
func TestErrorFrameShape(t *testing.T) {
	frames := runServer(t, []string{
		`{"cmd":"boom","task_id":1}`,
		`{"cmd":"notfound","task_id":2}`,
		`{"cmd":"pancake","task_id":3}`,
		`{"cmd":"badparams","task_id":4,"params":{"n":"这不是数字"}}`,
	}, func(s *Server) {
		s.Handle("boom", func(ctx context.Context, task *Task) (any, error) {
			return nil, Errorf(CodeNetwork, "连接超时")
		})
		s.Handle("pancake", func(ctx context.Context, task *Task) (any, error) {
			panic("故意炸一次")
		})
		s.Handle("badparams", func(ctx context.Context, task *Task) (any, error) {
			var v struct {
				N int `json:"n"`
			}
			if err := task.DecodeParams(&v); err != nil {
				return nil, err
			}
			return v, nil
		})
	})

	byTask := map[float64]map[string]any{}
	for _, f := range frames {
		if id, ok := f["task_id"].(float64); ok {
			byTask[id] = f
		}
	}

	check := func(id float64, wantCode string) {
		t.Helper()
		f, ok := byTask[id]
		if !ok {
			t.Fatalf("没有 task_id=%v 的帧", id)
		}
		// error 必须是字符串（宿主直接喂给 new Error()）
		msg, ok := f["error"].(string)
		if !ok || msg == "" {
			t.Fatalf("task_id=%v 的 error 不是非空字符串: %#v", id, f["error"])
		}
		if f["error_code"] != wantCode {
			t.Errorf("task_id=%v 的 error_code = %#v, 期望 %q", id, f["error_code"], wantCode)
		}
		if f["result"] != nil {
			t.Errorf("错误帧不应带 result: %v", f)
		}
		// error 文案里应当带上错误码，便于日志直接定位
		if !strings.Contains(msg, wantCode) {
			t.Errorf("error 文案 %q 未包含错误码 %q", msg, wantCode)
		}
	}

	check(1, CodeNetwork)
	check(2, CodeUnknownCommand)
	check(3, CodePanic)
	check(4, CodeInvalidParams)
}

// TestPanicDoesNotKillServer 确认单个 handler panic 不影响后续请求。
func TestPanicDoesNotKillServer(t *testing.T) {
	frames := runServer(t, []string{
		`{"cmd":"pancake","task_id":1}`,
		`{"cmd":"ok","task_id":2}`,
	}, func(s *Server) {
		s.Handle("pancake", func(ctx context.Context, task *Task) (any, error) {
			panic("boom")
		})
		s.Handle("ok", func(ctx context.Context, task *Task) (any, error) {
			return "survived", nil
		})
	})

	var survived bool
	for _, f := range frames {
		if f["task_id"] == float64(2) && f["result"] == "survived" {
			survived = true
		}
	}
	if !survived {
		t.Fatalf("panic 之后的请求没有被处理，服务端被拖垮了: %v", frames)
	}
}

// TestMalformedLineGoesToEventChannel 确认无法解析的行走事件通道。
//
// 连 task_id 都拿不到，没法归到任何任务上，只能用事件通知宿主。
func TestMalformedLineGoesToEventChannel(t *testing.T) {
	frames := runServer(t, []string{
		`{这不是 JSON`,
		`{"cmd":"ok","task_id":1}`,
	}, func(s *Server) {
		s.Handle("ok", func(ctx context.Context, task *Task) (any, error) { return "ok", nil })
	})

	var found bool
	for _, f := range frames {
		if f["event"] == EventProtocolError {
			found = true
			if _, bad := f["task_id"]; bad {
				t.Errorf("protocol-error 事件不应带顶层 task_id: %v", f)
			}
			data, _ := f["data"].(map[string]any)
			if data == nil || data["message"] == nil {
				t.Errorf("protocol-error 事件缺少 message: %v", f)
			}
		}
	}
	if !found {
		t.Fatalf("没有发出 protocol-error 事件: %v", frames)
	}
}

// TestReadyEventIsEmittedFirst 确认启动即发 ready，且 ready 不带顶层 task_id。
func TestReadyEventIsEmittedFirst(t *testing.T) {
	frames := runServer(t, []string{`{"cmd":"commands","task_id":1}`},
		func(s *Server) {
			s.Handle("dummy", func(ctx context.Context, task *Task) (any, error) { return nil, nil })
		})

	if len(frames) < 2 {
		t.Fatalf("帧数不足: %v", frames)
	}
	first := frames[0]
	if first["event"] != EventReady {
		t.Errorf("第一条帧不是 ready: %v", first)
	}
	if _, bad := first["task_id"]; bad {
		t.Errorf("ready 事件不应带顶层 task_id: %v", first)
	}
}

// TestCommandsIntrospection 确认 commands 内建命令能自省已注册的命令。
func TestCommandsIntrospection(t *testing.T) {
	frames := runServer(t, []string{`{"cmd":"commands","task_id":1}`},
		func(s *Server) {
			s.Handle("alpha", func(ctx context.Context, task *Task) (any, error) { return nil, nil })
			s.Handle("beta", func(ctx context.Context, task *Task) (any, error) { return nil, nil })
		})

	var res map[string]any
	for _, f := range frames {
		if f["task_id"] == float64(1) {
			res, _ = f["result"].(map[string]any)
		}
	}
	if res == nil {
		t.Fatalf("没有拿到 commands 的结果: %v", frames)
	}
	if res["version"] != Version {
		t.Errorf("version = %#v, 期望 %q", res["version"], Version)
	}
	cmds, _ := res["commands"].([]any)
	set := map[string]bool{}
	for _, c := range cmds {
		set[fmt.Sprint(c)] = true
	}
	if !set["alpha"] || !set["beta"] {
		t.Errorf("命令自省结果不完整: %v", cmds)
	}
}

// TestDuplicateHandlerPanics 确认重复注册会在启动期直接暴露。
func TestDuplicateHandlerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("重复注册同名命令应当 panic")
		}
	}()

	srv := NewServer(strings.NewReader(""), &bytes.Buffer{})
	h := func(ctx context.Context, task *Task) (any, error) { return nil, nil }
	srv.Handle("dup", h)
	srv.Handle("dup", h)
}

// TestCodeOfClassifiesErrors 确认错误码归类。
func TestCodeOfClassifiesErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{Errorf(CodeNetwork, "x"), CodeNetwork},
		{Wrap(CodeIO, errors.New("底层原因"), "包装"), CodeIO},
		{context.Canceled, CodeCanceled},
		{fmt.Errorf("包一层: %w", context.DeadlineExceeded), CodeTimeout},
		{errors.New("随便一个错"), CodeInternal},
	}
	for _, c := range cases {
		if got := CodeOf(c.err); got != c.want {
			t.Errorf("CodeOf(%v) = %q, 期望 %q", c.err, got, c.want)
		}
	}
}

// TestCodedInterface 确认外部错误类型只需实现 ErrorCode() 就能被识别。
func TestCodedInterface(t *testing.T) {
	err := fmt.Errorf("包一层: %w", &fakeCoded{code: CodeAPI})
	if got := CodeOf(err); got != CodeAPI {
		t.Errorf("CodeOf = %q, 期望 %q", got, CodeAPI)
	}
}

type fakeCoded struct{ code string }

func (e *fakeCoded) Error() string     { return "假错误" }
func (e *fakeCoded) ErrorCode() string { return e.code }

// TestRequestDecodeParamsNil 确认 params 缺省或为 null 时置零值而不报错。
//
// 宿主调用 call('probe') 时不传 params，Go 侧必须能处理。
func TestRequestDecodeParamsNil(t *testing.T) {
	cases := []string{
		`{"cmd":"x","task_id":1}`,
		`{"cmd":"x","task_id":1,"params":null}`,
		`{"cmd":"x","task_id":1,"params":{}}`,
	}
	for _, line := range cases {
		frames := runServer(t, []string{line}, func(s *Server) {
			s.Handle("x", func(ctx context.Context, task *Task) (any, error) {
				var v struct {
					A string `json:"a"`
				}
				if err := task.DecodeParams(&v); err != nil {
					return nil, err
				}
				return v.A, nil
			})
		})

		var ok bool
		for _, f := range frames {
			if f["task_id"] == float64(1) && f["error"] == nil {
				ok = true
			}
		}
		if !ok {
			t.Errorf("输入 %s 未被正常处理: %v", line, frames)
		}
	}
}
