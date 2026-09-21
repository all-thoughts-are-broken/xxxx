// Package service 把 client（接口）与 utils（下载/还原/PDF/渲染）编排成用例。
//
// 分层的边界：
//
//	protocol  只懂「一行 JSON 进、一行 JSON 出」，不认识业务
//	service   ← 本包：认识业务，但不认识 stdin/stdout
//	client    接口调用与解密
//	utils     纯算法：加解密、切图还原、下载、PDF、渲染
//
// 本包对外只暴露「一个请求 → 一个结果 + 若干进度」的形态，
// 不直接读写 os.Stdin/Stdout，便于单独测试。
//
// 关于 protocol：本包**只**用它来表达错误 —— 错误码常量（protocol.Code*）
// 与 Errorf/Wrap 构造器，目的是让宿主拿到能机器判别的 code，而不是一律 internal。
// 除此之外不碰协议层的任何东西（帧、Server、Task），也不认识 JSON 行协议。
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/client"
	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/utils"
)

// Progress 是一次进度上报。
type Progress struct {
	// Stage 阶段名：probe / download / restore / pdf / merge / encrypt / render。
	Stage string
	// Done 已完成数量。
	Done int
	// Total 总量；-1 表示未知。
	Total int
	// Message 人类可读的一句话，可为空。
	Message string
	// Extra 附加信息（当前项、输出路径、失败数等）。
	Extra map[string]any
}

// ProgressFunc 是进度回调。允许为 nil（表示调用方不关心进度）。
type ProgressFunc func(p Progress)

// report 安全地上报一次进度；f 为 nil 时什么都不做。
func (f ProgressFunc) report(p Progress) {
	if f == nil {
		return
	}
	f(p)
}

// step 是 report 的便捷写法：固定 stage 与总量，只报已完成数与文案。
func (f ProgressFunc) step(stage string, done, total int, message string) {
	f.report(Progress{Stage: stage, Done: done, Total: total, Message: message})
}

// Service 是业务编排入口，持有配置与共享的 API 客户端。
//
// 并发安全：client 的构建/换线走 mu 串行化，首次探活走 probeMu，
// 配置本身由 config.Store 保证。
type Service struct {
	cfg *config.Store

	mu     sync.Mutex
	cli    *client.Client
	cliKey string // 构建 cli 时所用网络配置的指纹，配置一变就重建

	// probeMu 串行化「第一次探活」。
	//
	// base_url 为空时每个并发请求都会走到探活分支，而探活要拉三个镜像的加密
	// 清单再并发测速五条线路 —— 不串行化就是每个请求各探一次（实测同时下发
	// 详情卡与评论图时，两个请求探到了**两条不同**的线路，最终用哪条取决于
	// 谁最后写）。串行化后只有第一个请求真探，其余在锁上等一小会儿直接复用。
	probeMu sync.Mutex
}

// New 创建服务实例。
func New(store *config.Store) *Service {
	s := &Service{cfg: store}
	// 字体是可选的：没有字体时渲染类命令会给出明确报错，其余命令不受影响。
	s.syncFonts(store.Get())
	return s
}

// Config 返回当前配置的快照。
func (s *Service) Config() *config.Config { return s.cfg.Get() }

// SetConfig 用 JSON 覆盖配置，并同步字体路径。
//
// 网络相关字段（base_url / secret / proxy 等）会在下一次请求时触发 client 重建，
// 无需调用方手动干预。
func (s *Service) SetConfig(raw []byte) (*config.Config, error) {
	cfg, err := s.cfg.Set(raw)
	if err != nil {
		return nil, err
	}
	s.syncFonts(cfg)
	return cfg, nil
}

// syncFonts 把配置里的字体路径解析成实际存在的文件并交给渲染层。
//
// 路径可能相对于可执行文件，也可能相对工作目录，所以走 config.ResolveFont。
func (s *Service) syncFonts(cfg *config.Config) {
	utils.SetupFonts(
		config.ResolveFont(cfg.FontRegular),
		config.ResolveFont(cfg.FontBold),
	)
}

// clientFor 返回与当前网络配置匹配的 client，必要时重建。
//
// 重建而不是「就地改字段」的原因：http.Client / Transport 一旦建好，
// 代理与超时都固化在连接池里，改字段不会生效，还会让在途请求拿着半新半旧的配置。
func (s *Service) clientFor(cfg *config.Config) (*client.Client, error) {
	key := clientKey(cfg)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cli != nil && s.cliKey == key {
		return s.cli, nil
	}

	cli, err := client.NewClient(
		client.WithBaseURL(cfg.BaseURL),
		client.WithAppVersion(cfg.AppVersion),
		client.WithSecret(cfg.Secret),
		client.WithProxy(cfg.Proxy),
		client.WithTimeout(cfg.Timeout()),
		client.WithUserAgent(cfg.UserAgent),
		client.WithCDNHost(cfg.CDNHost),
	)
	if err != nil {
		return nil, protocol.Wrap(protocol.CodeInvalidParams, err, "创建 API 客户端失败")
	}

	s.cli = cli
	s.cliKey = key
	return cli, nil
}

// clientKey 生成网络配置指纹。
func clientKey(cfg *config.Config) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%d|%s",
		cfg.BaseURL, cfg.Secret, cfg.AppVersion, cfg.UserAgent, cfg.Proxy, cfg.TimeoutSec, cfg.CDNHost)
}

// ensureClient 返回可用 client，并在 base_url 缺失时自动探活。
//
// 探活的成本是「每个进程一次」，结果只留在内存里、**不落盘**。这是刻意的：
//
//   - 线路会在几个月内整体失效（旧域名甚至还能回 HTTP 200 + 结构合法的信封，
//     只是解不开密）。一旦把探到的线路持久化，下次启动就走 `cfg.BaseURL != ""`
//     这条短路，**拿着一条死线路再也不探活**，工具直接卡死在那儿。
//   - 不落盘的代价只是再探一次（并发测速，秒级），换来的是每次都拿到活线路。
//
// 想跳过探活的宿主可以从 `ready` 事件或 `probe` 结果里拿到 base_url/cdn_host，
// 下次启动时随配置一起传进来 —— 那时短路是有意为之，因为线路由宿主掌握。
func (s *Service) ensureClient(ctx context.Context, prog ProgressFunc) (*client.Client, *config.Config, error) {
	// 快路径：已经探到线路，直接复用。绝大多数请求走这里。
	if cfg := s.cfg.Get(); cfg.BaseURL != "" {
		cli, err := s.clientForConfigured(cfg)
		if err != nil {
			return nil, nil, err
		}
		return cli, cfg, nil
	}

	// 慢路径：base_url 还是空的，需要真探一次。串行化理由见 Service.probeMu。
	s.probeMu.Lock()
	defer s.probeMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, nil, protocol.Wrap(protocol.CodeCanceled, err, "等待线路探活时请求已取消")
	}

	// 等锁期间可能已经被别人探好了，再查一次（double-check）。
	cfg := s.cfg.Get()
	if cfg.BaseURL != "" {
		cli, err := s.clientForConfigured(cfg)
		if err != nil {
			return nil, nil, err
		}
		return cli, cfg, nil
	}

	cli, err := s.clientFor(cfg)
	if err != nil {
		return nil, nil, err
	}

	// 先拿官方动态线路清单，再并发测延迟取最快。
	res, err := s.probeLines(ctx, cli, cfg, prog)
	if err != nil {
		return nil, nil, err
	}

	// 把探到的线路写回内存配置，本次进程内后续请求就不用再探。
	// 不写磁盘 —— 理由见函数注释。
	if updated, err := s.cfg.Set(mustJSON(map[string]any{
		"base_url": res.BaseURL,
		"cdn_host": res.CDNHost,
	})); err == nil {
		cfg = updated
		// 指纹变了（base_url/cdn_host 刚被填上），重建一次让它们进 client 字段。
		if cli2, err2 := s.clientFor(cfg); err2 == nil {
			cli = cli2
			cli.SetBaseURL(cfg.BaseURL)
			cli.SetCDNHost(cfg.CDNHost)
		}
	}

	return cli, cfg, nil
}

// clientForConfigured 返回一个 base_url / cdn_host 已按配置就位的 client。
//
// base_url 为空时不动 client 的线路线段 —— 探活前的调用方就指望它保持现状。
func (s *Service) clientForConfigured(cfg *config.Config) (*client.Client, error) {
	cli, err := s.clientFor(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.BaseURL != "" {
		cli.SetBaseURL(cfg.BaseURL)
		if cfg.CDNHost != "" {
			cli.SetCDNHost(cfg.CDNHost)
		}
	}
	return cli, nil
}

// probeLines 选出可用线路：优先用官方动态清单，并取延迟最低的那条。
//
// 为什么不直接顺序试硬编码列表：
//   - 线路是官方会轮换的，硬编码列表迟早整体失效。更阴的是失效方式 ——
//     旧域名可能仍然返回 HTTP 200 和结构合法的信封，只是响应解不开，
//     看起来像"加解密算法坏了"（实测 cdnbea 就是这种）。
//   - 线路之间的延迟能差一倍以上，而下载一本作品要打几百次接口，
//     所以值得把候选全部并发试一遍取最快的。
//
// 清单拉不到不算致命：退回内置兜底列表顺序试，命中即停。
func (s *Service) probeLines(
	ctx context.Context, cli *client.Client, cfg *config.Config, prog ProgressFunc,
) (client.ProbeResult, error) {
	// 清单放在第三方对象存储上，单个镜像超时收紧一点：一个镜像卡住
	// 不该让整个探活等满业务超时。
	listTimeout := cfg.Timeout()
	if listTimeout > 15*time.Second {
		listTimeout = 15 * time.Second
	}

	prog.report(Progress{Stage: "probe", Total: 1, Message: "正在获取线路清单"})

	var (
		candidates   []string
		fromNodeList bool
	)
	if list, err := client.FetchNodeList(ctx, cfg.Proxy, nil, listTimeout); err == nil {
		candidates = list.Hosts()
		fromNodeList = true
		prog.report(Progress{
			Stage:   "probe",
			Done:    1,
			Total:   1,
			Message: fmt.Sprintf("线路清单：%d 条候选", len(candidates)),
			Extra:   map[string]any{"nodes": candidates},
		})
	} else {
		candidates = append([]string(nil), config.DefaultBaseURLs...)
		prog.report(Progress{
			Stage:   "probe",
			Done:    1,
			Total:   1,
			Message: "线路清单获取失败，改用内置兜底列表：" + err.Error(),
			Extra:   map[string]any{"nodes": candidates},
		})
	}

	prog.report(Progress{Stage: "probe", Total: len(candidates), Message: "正在并发测速挑最低延迟"})

	var res client.ProbeResult
	if fromNodeList {
		// 清单里的线路都是当前有效的，全量并发测速取最快。
		res = cli.ProbeFastest(ctx, candidates, cfg.ProbeAID, 0)
	} else {
		// 兜底列表掺着失效线路，顺序试更省事：命中即停。
		res = cli.ProbeBaseURLs(ctx, candidates, cfg.ProbeAID)
	}

	if !res.OK() {
		return res, protocol.Errorf(protocol.CodeNetwork, "线路探活失败: %s", res.Error())
	}

	lines := make([]map[string]any, 0, len(res.Attempts))
	for _, a := range res.Attempts {
		item := map[string]any{"base_url": a.BaseURL, "latency_ms": a.LatencyMS, "ok": a.Err == nil}
		if a.Err != nil {
			item["error"] = a.Err.Error()
		}
		lines = append(lines, item)
	}

	prog.report(Progress{
		Stage:   "probe",
		Done:    1,
		Total:   1,
		Message: fmt.Sprintf("线路可用: %s", res.BaseURL),
		Extra: map[string]any{
			"base_url":   res.BaseURL,
			"cdn_host":   res.CDNHost,
			"lines":      lines,
			"from_nodes": fromNodeList,
		},
	})

	return res, nil
}

// EnsureClient 是对外暴露的探活入口，供「只想确认线路通不通」的宿主使用。
func (s *Service) EnsureClient(ctx context.Context, prog ProgressFunc) error {
	_, _, err := s.ensureClient(ctx, prog)
	return err
}
