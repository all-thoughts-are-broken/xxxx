package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	// defaultMaxLineBytes 单行请求上限。批量任务（如一次传几百张图的还原清单）会比较大。
	defaultMaxLineBytes = 32 << 20 // 32 MiB
	// defaultMaxConcurrency 同时在跑的请求数上限。
	//
	// 注意：这里的"并发"指宿主同时下发了多少个请求，不是下载/还原的线程数——
	// 后者由各命令自己的 workers 参数控制。设这个闸门是为了防止宿主一次性
	// 灌进来几十个重任务把内存打爆。
	defaultMaxConcurrency = 8
	// shutdownGrace 收到 shutdown 后，留给宿主读走响应帧的时间。
	shutdownGrace = 80 * time.Millisecond
)

// Handler 处理一个命令。返回的 result 会被放进结果帧；
// 返回 error 时按 CodeOf 生成错误帧。
//
// ctx 会在宿主取消该任务或进程退出时结束，长耗时操作应当一路透传下去。
type Handler func(ctx context.Context, t *Task) (any, error)

// Task 是单次请求的执行上下文，供 handler 上报进度与读取参数。
type Task struct {
	Request

	ctx    context.Context
	emit   func(Event)
	logger *log.Logger
}

// Context 返回任务上下文。
func (t *Task) Context() context.Context { return t.ctx }

// Progress 上报一条进度事件。
func (t *Task) Progress(stage string, done, total int, message string, extra map[string]any) {
	t.emit(Event{
		Event: EventProgress,
		Data: &ProgressData{
			TaskID:  t.TaskID,
			Stage:   stage,
			Done:    done,
			Total:   total,
			Message: message,
			Extra:   extra,
		},
	})
}

// Progressf 是 Progress 的便捷写法，用 fmt 生成 message。
func (t *Task) Progressf(stage string, done, total int, format string, args ...any) {
	t.Progress(stage, done, total, fmt.Sprintf(format, args...), nil)
}

// Event 上报一个自定义事件。事件名由业务方约定。
func (t *Task) Event(name string, data any) {
	t.emit(Event{Event: name, Data: data})
}

// Logf 写一条日志到 stderr（绝不会污染 stdout 协议流）。
func (t *Task) Logf(format string, args ...any) { t.logger.Printf(format, args...) }

// Server 是 JSON 行协议服务端。
type Server struct {
	in       io.Reader
	out      io.Writer
	logger   *log.Logger
	handlers map[string]Handler

	writeMu sync.Mutex
	enc     *json.Encoder

	runMu sync.Mutex
	tasks map[string]context.CancelFunc

	wg        sync.WaitGroup
	sem       chan struct{}
	readyData any
}

// Option 配置 Server。
type Option func(*Server)

// WithLogger 指定日志出口，默认 stderr。
func WithLogger(l *log.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// WithMaxConcurrency 设置同时处理的请求数上限，n<=0 表示不限（仍受内存约束）。
func WithMaxConcurrency(n int) Option {
	return func(s *Server) {
		if n <= 0 {
			s.sem = nil
			return
		}
		s.sem = make(chan struct{}, n)
	}
}

// WithReadyData 定制 ready 事件携带的信息。
func WithReadyData(data any) Option {
	return func(s *Server) { s.readyData = data }
}

// NewServer 创建协议服务端。in/out 通常就是 os.Stdin / os.Stdout。
//
// out 必须是独占的：除本 Server 以外的任何代码都不许往这个 writer 写东西。
func NewServer(in io.Reader, out io.Writer, opts ...Option) *Server {
	s := &Server{
		in:       in,
		out:      out,
		logger:   log.New(os.Stderr, "", log.LstdFlags),
		handlers: make(map[string]Handler),
		tasks:    make(map[string]context.CancelFunc),
		sem:      make(chan struct{}, defaultMaxConcurrency),
	}
	for _, opt := range opts {
		opt(s)
	}
	enc := json.NewEncoder(out)
	// 帧内不要转义 HTML（作品名/评论里会带 < > & 之类的字符，转义后宿主还得再解码一次）
	enc.SetEscapeHTML(false)
	s.enc = enc
	return s
}

// Handle 注册一个命令。重复注册同名命令会 panic，属于编码期错误。
func (s *Server) Handle(cmd string, h Handler) {
	if _, dup := s.handlers[cmd]; dup {
		panic("protocol: 命令重复注册: " + cmd)
	}
	s.handlers[cmd] = h
}

// Commands 返回已注册的命令名，便于宿主自省。
func (s *Server) Commands() []string {
	out := make([]string, 0, len(s.handlers))
	for cmd := range s.handlers {
		out = append(out, cmd)
	}
	return out
}

// emitResult 写出结果帧。task_id 缺省时不会出现该字段。
func (s *Server) emitResult(id TaskID, result any) {
	s.write(Response{TaskID: taskIDPtr(id), Result: result})
}

// emitError 写出错误帧。err 中已含完整错误链，宿主拿到的是一段可直接展示的字符串。
func (s *Server) emitError(id TaskID, err error) {
	if err == nil {
		return
	}
	code := CodeOf(err)
	s.write(Response{
		TaskID:    taskIDPtr(id),
		Error:     fmt.Sprintf("%s: %s", code, MessageOf(err)),
		ErrorCode: code,
	})
}

// emitEvent 写出事件帧（不带顶层 task_id）。
func (s *Server) emitEvent(name string, data any) {
	s.write(Event{Event: name, Data: data})
}

// emitFrame 把已组装好的事件帧写出去，供 Task 内部使用。
//
// 单独留一个方法是因为 Task.emit 的签名是 func(Event)，而 emitEvent 是
// (name string, data any) —— Go 的函数类型不协变，不能直接把前者赋给她。
func (s *Server) emitFrame(ev Event) { s.write(ev) }

// write 串行化写出一个帧，保证多 goroutine 下不会把两行 JSON 交叉写到一起。
func (s *Server) write(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enc.Encode(v); err != nil {
		s.logger.Printf("写协议帧失败: %v", err)
	}
}

// Run 开始处理请求，直到 stdin 关闭、收到 shutdown 或 ctx 结束。
//
// stdin 关闭（宿主退出）时会等待在途任务收尾再返回，避免响应丢失。
func (s *Server) Run(ctx context.Context) error {
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	s.emitEvent(EventReady, s.readyData)

	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 64<<10), defaultMaxLineBytes)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			// 连 task_id 都拿不到，只能走事件通道告知，请求方自己认领。
			s.emitEvent(EventProtocolError, map[string]any{
				"message": fmt.Sprintf("请求不是合法 JSON: %v", err),
				"line":    truncate(string(line), 500),
			})
			continue
		}
		req.Raw = append(json.RawMessage(nil), line...)

		if len(req.Params) > 0 && bytes.Equal(bytes.TrimSpace(req.Params), []byte("null")) {
			req.Params = nil
		}

		if s.handleControl(req) {
			continue
		}
		if strings.TrimSpace(req.Cmd) == "" {
			s.emitError(req.TaskID, Errorf(CodeBadRequest, "缺少 cmd 字段"))
			continue
		}
		s.dispatch(ctx, req)
	}

	if err := scanner.Err(); err != nil {
		return Wrap(CodeIO, err, "读取 stdin 失败")
	}

	// stdin 已关闭：等在途任务跑完，让它们有机会把结果帧写出去。
	s.wg.Wait()
	return nil
}

// handleControl 处理协议内建命令，返回 true 表示已消费该请求。
func (s *Server) handleControl(req Request) bool {
	switch req.Cmd {
	case "cancel":
		n := s.cancel(req.TaskID)
		s.emitResult(req.TaskID, map[string]any{"canceled": n})
		return true

	case "commands":
		s.emitResult(req.TaskID, map[string]any{
			"version":  Version,
			"commands": s.Commands(),
		})
		return true

	case "shutdown":
		// 契约来自 CoreBridge.shutdown()：先回一个结果，宿主拿到后进程再退出。
		s.emitResult(req.TaskID, "bye")
		go s.gracefulExit()

		return true
	}
	return false
}

// gracefulExit 取消所有在途任务，给它们一点收尾时间，然后退出进程。
//
// 只用于收到 shutdown 时。stdin 被关闭的正常路径由 Run 的 wg.Wait() 负责。
func (s *Server) gracefulExit() {
	time.Sleep(shutdownGrace)

	s.cancel(TaskID{})

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.logger.Printf("shutdown: 等待在途任务超时，强制退出")
	}
	os.Exit(0)
}

// cancel 取消指定任务；task_id 缺省时取消全部在途任务。返回被取消的数量。
func (s *Server) cancel(id TaskID) int {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if id.IsZero() {
		n := len(s.tasks)
		for k, cancel := range s.tasks {
			cancel()
			delete(s.tasks, k)
		}
		return n
	}

	key := id.String()
	cancel, ok := s.tasks[key]
	if !ok {
		return 0
	}
	cancel()
	delete(s.tasks, key)
	return 1
}

// dispatch 取闸门、登记可取消入口，然后起 goroutine 执行。
func (s *Server) dispatch(ctx context.Context, req Request) {
	h, ok := s.handlers[req.Cmd]
	if !ok {
		s.emitError(req.TaskID, Errorf(CodeUnknownCommand, "未知命令 %q", req.Cmd))
		return
	}

	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			s.emitError(req.TaskID, Wrap(CodeCanceled, ctx.Err(), "进程正在退出，请求未执行"))
			return
		}
	}

	taskCtx, cancel := context.WithCancel(ctx)
	key := req.TaskID.String()

	s.runMu.Lock()
	s.tasks[key] = cancel
	s.runMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if s.sem != nil {
			defer func() { <-s.sem }()
		}
		defer func() {
			cancel()
			s.runMu.Lock()
			delete(s.tasks, key)
			s.runMu.Unlock()
		}()

		t := &Task{Request: req, ctx: taskCtx, emit: s.emitFrame, logger: s.logger}
		s.invoke(taskCtx, t, h)
	}()
}

// invoke 执行 handler 并把 panic 收敛成一个错误帧，避免单个请求拖垮整个进程。
func (s *Server) invoke(ctx context.Context, t *Task, h Handler) {
	var (
		result any
		err    error
	)

	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			stack := debug.Stack()
			s.logger.Printf("任务 %s 执行 %s 时 panic: %v\n%s", t.TaskID.String(), t.Cmd, r, stack)
			err = Errorf(CodePanic, "panic: %v", r)
		}()
		result, err = h(ctx, t)
	}()

	if err != nil {
		s.emitError(t.TaskID, err)
		return
	}
	s.emitResult(t.TaskID, result)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
