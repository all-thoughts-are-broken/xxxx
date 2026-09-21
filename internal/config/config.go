// Package config 管理运行期配置。
//
// 优先级：内置默认值 < 配置文件 < 宿主通过 config_set 命令下发的运行时覆盖。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/all-thoughts-are-broken/xxxx/internal/protocol"
)

// 与 JM 官方 APP 对齐的默认常量（取自 app 内置静态配置）。
const (
	// DefaultScrambleID 220980 起作品开始切图；更早的作品图片是完整的。
	DefaultScrambleID = 220980
	// DefaultAppVersion APP 版本号，参与 token 签名。
	DefaultAppVersion = "1.8.0"
	// DefaultSecret 响应体 AES 密钥的盐（appDataSecret）。
	DefaultSecret = "185Hcomic3PAPP7R"
	// DefaultUserAgent 与 APP 请求头保持一致，避免被风控拦。
	DefaultUserAgent = "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/116.0.0.0 Mobile Safari/537.36"
	// DefaultProbeAID 用于探测线路与 CDN 域名的已知存在作品。
	DefaultProbeAID = 10086
)

// DefaultBaseURLs 是**兜底**用的候选 API 线路。
//
// 正常情况下不该靠这份硬编码列表：线路清单本身是动态的，官方会通过
// 三份加密的 newsvr-2025.txt 镜像下发（见 internal/client/nodes.go），
// 由它给出当前可用节点并按延迟挑最快的。这里的列表只在清单拉取失败时兜底。
//
// 列表会过期，别指望它长期有效。2026-09-21 实测的结论：
//   - cdnhjk/cdngwc/cdnutc 这批是当前有效节点（来自当时的 newsvr-2025.txt）
//   - 旧的 cdnaspa / cdnplaystation6 系列已全部作废：分别返回 404（Apache）、
//     502 Bad Gateway，或干脆回一个「發布頁」HTML —— 那些域名已改作官网。
//   - cdnbea 最阴：HTTP 200、信封结构完全合法，所以看起来"通了"，
//     但它是废弃线路，响应解不开（表现为「去除填充失败: 非法的填充长度 N」）。
//     —— 探活必须真的解一次密，只看状态码会被它骗过去。
var DefaultBaseURLs = []string{
	"https://www.cdngwc.cc",
	"https://www.cdnhjk.net",
	"https://www.cdngwc.net",
	"https://www.cdngwc.club",
	"https://www.cdnutc.me",
	// ---- 以下为历史线路，多数已失效，留着当最后的兜底 ----
	"https://www.cdnbea.net",
	"https://www.cdnaspa.vip",
	"https://www.cdnplaystation6.cc",
}

// DefaultPDFMaxPageHeightPt 单个 PDF 页面高度的上限（点）；0 = 不限制。
//
// 产品形态是「一章 = 一个只有一页的长 PDF」，所以默认不限制。
// 如果宿主遇到阅读器拒绝打开超长页（Acrobat 上限 200 英寸 = 14400pt），
// 可以把 pdf_max_page_height 设为 14400 打开上限。
// 写成 0.0 而不是 0：这个常量要赋给 float64 字段，无类型的 0 在被塞进 any
// 做比较时会变成 int，和字段的 float64 不是同一个值（测试里踩过）。
const DefaultPDFMaxPageHeightPt = 0.0

// Config 是完整运行配置。字段全部带 json tag，可直接被 config_get / config_set 复用。
type Config struct {
	// ---- 网络 ----
	Proxy      string `json:"proxy"`       // http/https/socks5 代理，空表示直连
	BaseURL    string `json:"base_url"`    // API 线路，空则启动时自动探活
	CDNHost    string `json:"cdn_host"`    // 图片 CDN 域名，空则从阅读页响应里提取
	AppVersion string `json:"app_version"` // APP 版本号
	Secret     string `json:"secret"`      // 响应解密盐
	UserAgent  string `json:"user_agent"`
	TimeoutSec int    `json:"timeout_sec"` // 单次接口请求超时（秒）
	ProbeAID   int    `json:"probe_aid"`   // 线路/CDN 探活用作品号

	// ---- 下载 ----
	DownloadDir  string `json:"download_dir"`  // 原始（未还原）图片落盘目录
	OutputDir    string `json:"output_dir"`    // 还原后图片/PDF 输出目录
	Workers      int    `json:"workers"`       // 批量下载/还原的并发数
	MaxRetries   int    `json:"max_retries"`   // 单文件下载重试次数
	SkipExisting bool   `json:"skip_existing"` // 已存在且非空的文件直接跳过（断点续传）
	ScrambleID   int    `json:"scramble_id"`   // 默认切图阈值，可被阅读页响应覆盖

	// ---- PDF ----
	PDFPassword      string  `json:"pdf_password"`        // 用户密码，空则不加密
	PDFOwnerPassword string  `json:"pdf_owner_password"`  // 所有者密码，空则与用户密码相同
	PDFMaxPageHeight float64 `json:"pdf_max_page_height"` // 单页最大高度（pt）
	PDFLayout        string  `json:"pdf_layout"`          // single=整本一条长页 / paged=按上限分页
	// PDFPerImageBookmark 是否为每一页建二级书签（挂在章节书签下）。
	//
	// 默认开启：一章虽然只占一页长页，但页数/页序本身就是有用信息，
	// 而且参考实现（pdf_test）产出的 PDF 就是这个结构。
	// 有些阅读器在大纲节点特别多时会卡（长章节几百页），宿主可关掉。
	PDFPerImageBookmark bool `json:"pdf_per_image_bookmark"`

	// ---- 渲染 ----
	FontRegular string `json:"font_regular"`
	FontBold    string `json:"font_bold"`
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Proxy:               "",
		BaseURL:             "",
		CDNHost:             "",
		AppVersion:          DefaultAppVersion,
		Secret:              DefaultSecret,
		UserAgent:           DefaultUserAgent,
		TimeoutSec:          30,
		ProbeAID:            DefaultProbeAID,
		DownloadDir:         "download",
		OutputDir:           "output",
		Workers:             4,
		MaxRetries:          3,
		SkipExisting:        true,
		ScrambleID:          DefaultScrambleID,
		PDFMaxPageHeight:    DefaultPDFMaxPageHeightPt,
		PDFLayout:           "single",
		PDFPerImageBookmark: true,
		FontRegular:         filepath.Join("fonts", "NotoSansSC-Regular.ttf"),
		FontBold:            filepath.Join("fonts", "Noto-Sans-SC-Bold-2.ttf"),
	}
}

// Timeout 返回请求超时。
func (c *Config) Timeout() time.Duration {
	if c.TimeoutSec <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.TimeoutSec) * time.Second
}

// EffectiveWorkers 返回实际并发数（至少 1）。
func (c *Config) EffectiveWorkers() int {
	if c.Workers <= 0 {
		return 1
	}
	return c.Workers
}

// EffectiveMaxRetries 返回实际重试次数（至少 0）。
func (c *Config) EffectiveMaxRetries() int {
	if c.MaxRetries < 0 {
		return 0
	}
	return c.MaxRetries
}

// Validate 归一化并校验配置，返回修正过的副本。
func (c *Config) Validate() error {
	if c.AppVersion == "" {
		c.AppVersion = DefaultAppVersion
	}
	if c.Secret == "" {
		return protocol.Errorf(protocol.CodeInvalidParams, "secret 不能为空，否则无法解密响应")
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = 30
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.PDFMaxPageHeight < 0 {
		c.PDFMaxPageHeight = 0 // 0 = 不限制页高
	}
	switch c.PDFLayout {
	case "", "single":
		c.PDFLayout = "single"
	case "paged":
	default:
		return protocol.Errorf(protocol.CodeInvalidParams, "未知的 pdf_layout: %q（只能是 single 或 paged）", c.PDFLayout)
	}
	if c.ScrambleID <= 0 {
		c.ScrambleID = DefaultScrambleID
	}
	if c.ProbeAID <= 0 {
		c.ProbeAID = DefaultProbeAID
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.CDNHost = strings.TrimSpace(c.CDNHost)
	return nil
}

// Set 用一份 JSON 对象覆盖当前配置的对应字段（用于 config_set 命令）。
//
// 只覆盖显式出现的字段，未出现的保持原值；同时拒绝未知字段，避免拼错了却静默生效。
func (c *Config) Set(raw json.RawMessage) error {
	if len(raw) == 0 {
		return protocol.Errorf(protocol.CodeInvalidParams, "缺少 params")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return protocol.Wrap(protocol.CodeInvalidParams, err, "配置字段不合法")
	}
	return c.Validate()
}

// Store 是并发安全的配置容器。
type Store struct {
	mu  sync.RWMutex
	cfg *Config
}

// NewStore 用给定配置创建容器；cfg 为 nil 时使用默认值。
//
// 这里丢掉 Validate 的返回值是**刻意的**：Load / Save 那条路上已经校验并
// 传播过错误了（文件里的坏值在那儿就会被拒），Default() 本身也一定合法。
// 这里再调一次只是为了「就地补默认值」——Validate 会把 AppVersion / Timeout /
// Workers 之类的零值填成默认，免得下游到处判零。
//
// 已知取舍：直接 NewStore 一个手搓的非法配置（比如未知的 pdf_layout）不会
// 在这里报错，而是带着非法值走下去。需要严格校验的入口自己先调 Validate。
func NewStore(cfg *Config) *Store {
	if cfg == nil {
		cfg = Default()
	}
	_ = cfg.Validate()
	return &Store{cfg: cfg}
}

// Get 返回配置的快照副本，调用方可以随意改写而不会影响全局。
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := *s.cfg
	return &cp
}

// Set 用 JSON 覆盖配置，成功时返回覆盖后的快照。
func (s *Store) Set(raw json.RawMessage) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := *s.cfg
	if err := next.Set(raw); err != nil {
		return nil, err
	}
	s.cfg = &next
	return &next, nil
}

// Load 从文件读取配置。文件不存在时返回默认配置且不报错。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, protocol.Wrap(protocol.CodeIO, err, "读取配置文件 %s", path)
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, protocol.Wrap(protocol.CodeInvalidParams, err, "解析配置文件 %s", path)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save 把配置写回文件（临时文件 + rename，避免写一半被打断留下坏文件）。
func Save(path string, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return protocol.Wrap(protocol.CodeInternal, err, "序列化配置")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return protocol.Wrap(protocol.CodeIO, err, "创建配置目录 %s", dir)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return protocol.Wrap(protocol.CodeIO, err, "写入临时配置 %s", tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return protocol.Wrap(protocol.CodeIO, err, "替换配置文件 %s", path)
	}
	return nil
}

// ResolveFont 返回第一个存在的字体路径，都不存在时返回空串。
//
// 依次尝试：配置里写的路径、可执行文件同级的同名文件、当前工作目录下的同名文件。
// 这样无论从仓库根目录还是从 bin/ 目录启动都能找到字体。
func ResolveFont(configured string) string {
	if configured == "" {
		return ""
	}
	candidates := []string{configured}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), configured))
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), filepath.Base(configured)))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// String 便于日志输出（隐去 secret）。
func (c *Config) String() string {
	masked := c.Secret
	if masked != "" {
		masked = "***"
	}
	return fmt.Sprintf("base_url=%q cdn_host=%q workers=%d proxy=%q secret=%s",
		c.BaseURL, c.CDNHost, c.Workers, c.Proxy, masked)
}
