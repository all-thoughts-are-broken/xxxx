// Command xxxx 是漫画下载/还原/合成工具的 JSON 行协议服务端。
//
// 它是一个「被动进程」：不解析命令行参数，启动后从 stdin 逐行读取 JSON 请求，
// 把结果与进度写到 stdout（stderr 留给日志）。宿主（如 Node 的 CoreBridge）
// spawn 它之后即可双向通信。
//
// 协议细节见 internal/protocol 的包注释；业务能力见 internal/service。
//
// 本文件只做装配：注册命令、把协议层的 Task 适配成 service 的进度回调。
// 所有业务逻辑都在 internal/service，所有算法在 internal/utils。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/all-thoughts-are-broken/xxxx/internal/config"
	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
	"github.com/all-thoughts-are-broken/xxxx/internal/service"
)

// 命令名常量。
//
// 前六个是初版就定下的名字（宿主可能已经按它们写好了调用），保持不变；
// 后面几个是这次补的能力。
const (
	cmdConfigGet     = "config_get"
	cmdConfigSet     = "config_set"
	cmdProbe         = "probe"
	cmdAlbumDetail   = "get_album_detail"
	cmdAlbumComment  = "get_album_comment"
	cmdComicRead     = "get_comic_read"
	cmdDownloadAlbum = "download_album"
	cmdRestoreImages = "restore_images"
	cmdConvertToPDF  = "convert_images_to_PDF"
	cmdMergePDF      = "merge_pdf"
	cmdEncryptPDF    = "encrypt_pdf"
	cmdRenderDetail  = "render_album_detail_image"
	cmdRenderComment = "render_album_comment_image"
)

func main() {
	// ---- 把 stdout 保护起来 ----
	//
	// 本进程的 stdout 是协议通道：只能出现一行行 JSON 帧。但第三方库
	// （pdfcpu / gofpdf / 标准库的 fmt.Println）随时可能往 os.Stdout 直接打印，
	// 一旦发生，宿主侧就会解析到垃圾行、整个 promise 链错乱。
	//
	// 做法：先把真正的 stdout 抓在手里交给协议层独占，然后把 os.Stdout
	// 重新指向 stderr。此后任何「往 os.Stdout 打印」的代码都只会把内容
	// 写进日志通道，不会污染协议。
	realStdout := os.Stdout
	os.Stdout = os.Stderr

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[jm] ")

	if err := run(realStdout); err != nil {
		// 走到这里说明服务端本身起不来（配置坏、注册重名等），
		// 宿主只会看到进程退出，所以必须把原因留在 stderr。
		log.Printf("致命错误: %v", err)
		os.Exit(1)
	}
}

// run 装配并启动协议服务端。
func run(protocolOut *os.File) error {
	store, err := loadConfig()
	if err != nil {
		return err
	}
	svc := service.New(store)

	srv := protocol.NewServer(os.Stdin, protocolOut,
		protocol.WithReadyData(map[string]any{
			"pid":      os.Getpid(),
			"version":  protocol.Version,
			"base_url": store.Get().BaseURL,
			"cdn_host": store.Get().CDNHost,
		}),
	)

	registerHandlers(srv, svc)

	log.Printf("就绪 pid=%d 配置=%s", os.Getpid(), store.Get())
	return srv.Run(context.Background())
}

// loadConfig 定位并读取配置文件。
//
// 查找顺序：
//
//	$JM_CONFIG               显式指定
//	<可执行文件同级>/config.json
//	<当前工作目录>/config.json
//
// 文件不存在不是错误（返回默认配置）：首次运行的用户不该被迫先写一个配置文件。
func loadConfig() (*config.Store, error) {
	path := os.Getenv("JM_CONFIG")

	if path == "" {
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), "config.json")
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
			}
		}
	}
	if path == "" {
		if _, err := os.Stat("config.json"); err == nil {
			path = "config.json"
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("加载配置 %s: %w", path, err)
	}

	// 字体路径可能是相对仓库根目录写的，而从 bin/ 启动时相对路径就失效了。
	// 这里统一按「配置里写的 → 可执行文件同级 → 工作目录」的顺序解析一次。
	cfg.FontRegular = config.ResolveFont(cfg.FontRegular)
	cfg.FontBold = config.ResolveFont(cfg.FontBold)

	store := config.NewStore(cfg)
	if path != "" {
		log.Printf("配置来源: %s", path)
	}
	return store, nil
}

// registerHandlers 注册全部命令。
func registerHandlers(srv *protocol.Server, svc *service.Service) {
	// ---- 配置 ----
	handle(srv, cmdConfigGet, func(ctx context.Context, _ service.ProgressFunc, _ emptyReq) (any, error) {
		return svc.Config(), nil
	})

	handle(srv, cmdConfigSet, func(ctx context.Context, _ service.ProgressFunc, req setConfigReq) (any, error) {
		cfg, err := svc.SetConfig(req.Raw)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	})

	// ---- 线路探活 ----
	handle(srv, cmdProbe, func(ctx context.Context, prog service.ProgressFunc, _ emptyReq) (any, error) {
		if err := svc.EnsureClient(ctx, prog); err != nil {
			return nil, err
		}
		cfg := svc.Config()
		return map[string]any{
			"base_url": cfg.BaseURL,
			"cdn_host": cfg.CDNHost,
		}, nil
	})

	// ---- 接口查询 ----
	handle(srv, cmdAlbumDetail, svc.AlbumDetail)
	handle(srv, cmdAlbumComment, svc.AlbumComments)
	handle(srv, cmdComicRead, svc.ReadPage)

	// ---- 下载 ----
	handle(srv, cmdDownloadAlbum, svc.DownloadAlbum)

	// ---- 本地处理 ----
	handle(srv, cmdRestoreImages, svc.RestoreImages)
	handle(srv, cmdConvertToPDF, svc.ConvertImagesToPDF)
	handle(srv, cmdMergePDF, svc.MergePDFs)
	handle(srv, cmdEncryptPDF, svc.EncryptPDF)

	// ---- 渲染 ----
	handle(srv, cmdRenderDetail, svc.RenderAlbumDetailImage)
	handle(srv, cmdRenderComment, svc.RenderAlbumCommentImage)
}

// emptyReq 用于「不需要参数」的命令。
type emptyReq struct{}

// setConfigReq 之所以不直接解码成 config.Config，是因为要支持「只传部分字段」
// 的增量覆盖 —— 交给 config.Store.Set 处理，它拒绝了未知字段。
type setConfigReq struct {
	Raw []byte `json:"-"`
}

func (r *setConfigReq) UnmarshalJSON(b []byte) error {
	r.Raw = append(r.Raw[:0], b...)
	return nil
}

// serviceFunc 是 service 层用例的统一形态。
type serviceFunc[Req, Res any] func(ctx context.Context, prog service.ProgressFunc, req Req) (Res, error)

// handle 把 service 的用例适配成一个 protocol handler。
//
// 三件事在这里一次性做掉，业务代码里就不用反复写了：
//
//  1. params → 具体请求结构体（失败即返回 invalid_params）
//  2. protocol.Task → service.ProgressFunc（进度事件透传，task_id 由协议层补进 data）
//  3. 结果直接交给协议层序列化
func handle[Req, Res any](srv *protocol.Server, cmd string, fn serviceFunc[Req, Res]) {
	srv.Handle(cmd, func(ctx context.Context, t *protocol.Task) (any, error) {
		var req Req

		// 少数命令的参数不是「字段对象」而是原始 JSON（config_set 就是），
		// 它们自己实现了 UnmarshalJSON，直接喂原始 params。
		if raw, ok := any(&req).(rawParams); ok {
			if len(t.Params) > 0 {
				if err := raw.UnmarshalJSON(t.Params); err != nil {
					return nil, protocol.Wrap(protocol.CodeInvalidParams, err, "params 解析失败")
				}
			}
		} else if err := t.DecodeParams(&req); err != nil {
			return nil, err
		}

		return fn(ctx, taskProgress(t), req)
	})
}

// rawParams 由「想拿到未经结构化解码的原始 params」的请求类型实现。
type rawParams interface {
	UnmarshalJSON([]byte) error
}

// taskProgress 把协议层的 Task 适配成 service 的进度回调。
//
// 进度直接复用 Task.Progress —— 它会把 task_id 放进 data 里（而不是顶层），
// 这正是 CoreBridge 区分「事件」与「响应」的依据。
func taskProgress(t *protocol.Task) service.ProgressFunc {
	return func(p service.Progress) {
		t.Progress(p.Stage, p.Done, p.Total, p.Message, p.Extra)
	}
}
