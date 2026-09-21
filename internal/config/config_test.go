package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ---- 默认值 ----

// TestDefaultValues 把「出厂默认」钉住。
//
// 不是为了防手滑改数字，而是因为其中几条**语义上是承诺**：
// 一章一条长页（0 = 不限高）、默认不加密、断点续传默认开、
// 页码书签默认开。改这些等于改产品形态，应该是有意识的决定 + 改测试。
func TestDefaultValues(t *testing.T) {
	c := Default()

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"AppVersion", c.AppVersion, DefaultAppVersion},
		{"Secret", c.Secret, DefaultSecret},
		{"ScrambleID", c.ScrambleID, DefaultScrambleID},
		{"ProbeAID", c.ProbeAID, DefaultProbeAID},
		{"TimeoutSec", c.TimeoutSec, 30},
		{"Workers", c.Workers, 4},
		{"MaxRetries", c.MaxRetries, 3},
		{"SkipExisting", c.SkipExisting, true},
		// 「一章 = 一个只有一页的长 PDF」：0 表示不限高，别改成有限值。
		{"PDFMaxPageHeight", c.PDFMaxPageHeight, DefaultPDFMaxPageHeightPt},
		{"PDFLayout", c.PDFLayout, "single"},
		// 章节下面的页码书签默认要有（master 明确要求）。
		{"PDFPerImageBookmark", c.PDFPerImageBookmark, true},
		// 默认不加密。
		{"PDFPassword", c.PDFPassword, ""},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("默认值 %s = %v, 期望 %v", tc.name, tc.got, tc.want)
		}
	}
}

// ---- Load ----

// TestLoadKeepsBuiltInDefaultsForMissingKeys 锁住 Load 的实现方式：
// 它必须是「在 Default() 之上覆盖」，不能是「unmarshal 进零值 Config」。
//
// 这条很容易在重构时被"简化"掉，而后果是**静默的**：
// 老用户的配置文件里没有新加的键，于是新键全部退回零值 ——
// skip_existing 变成 false（每次全量重下）、pdf_per_image_bookmark 变成 false
// （页码书签又消失）、workers 变成 0。没有报错，只有行为和文档不一致。
func TestLoadKeepsBuiltInDefaultsForMissingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// 一份"老版本写出来的"配置：只有当时存在的键。
	partial := `{
		"base_url": "https://example.invalid",
		"output_dir": "out",
		"workers": 8
	}`
	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	// 文件里写了的，按文件来。
	if c.BaseURL != "https://example.invalid" || c.OutputDir != "out" || c.Workers != 8 {
		t.Errorf("文件里显式给的值没生效: base_url=%q output_dir=%q workers=%d",
			c.BaseURL, c.OutputDir, c.Workers)
	}

	// 文件里没写的新键，必须还是内置默认值，而不是零值。
	if !c.PDFPerImageBookmark {
		t.Error("pdf_per_image_bookmark 退回了零值 —— 缺失的键必须保留内置默认 true")
	}
	if !c.SkipExisting {
		t.Error("skip_existing 退回了零值 —— 会变成每次都全量重下")
	}
	if c.PDFLayout != "single" || c.PDFMaxPageHeight != DefaultPDFMaxPageHeightPt {
		t.Errorf("PDF 默认被零值覆盖: layout=%q maxH=%v", c.PDFLayout, c.PDFMaxPageHeight)
	}
	if c.Secret != DefaultSecret || c.AppVersion != DefaultAppVersion {
		t.Errorf("网络默认被零值覆盖: secret=%q version=%q", c.Secret, c.AppVersion)
	}
	if c.DownloadDir == "" || c.MaxRetries == 0 {
		t.Errorf("下载默认被零值覆盖: download_dir=%q max_retries=%d", c.DownloadDir, c.MaxRetries)
	}
}

// TestLoadMissingFileIsNotAnError：首次运行的用户不该被迫先写配置文件。
func TestLoadMissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("文件不存在不应报错: %v", err)
	}
	if c == nil || c.Secret != DefaultSecret {
		t.Fatalf("应返回默认配置，实际: %+v", c)
	}
}

// TestLoadEmptyPathUsesDefaults：不指定路径（无配置文件）时给默认值。
func TestLoadEmptyPathUsesDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
	if !c.PDFPerImageBookmark || c.PDFLayout != "single" {
		t.Errorf("空路径应返回默认配置，实际 layout=%q bookmark=%v", c.PDFLayout, c.PDFPerImageBookmark)
	}
}

func TestLoadRejectsBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{ 这不是 json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("坏 JSON 应当报错")
	}
}

// ---- Set（config_set 的语义）----

// TestSetIsPatchNotReplace：config_set 必须只覆盖给到的字段。
//
// 整体替换会让宿主"只想改个 workers"就把 base_url / 目录全清掉，
// 而且这种丢配置很难被发现（下一次请求才炸）。
func TestSetIsPatchNotReplace(t *testing.T) {
	store := NewStore(Default())

	base, err := store.Set(json.RawMessage(`{"base_url":"https://a.example","cdn_host":"https://c.example"}`))
	if err != nil {
		t.Fatalf("patch 失败: %v", err)
	}
	if base.BaseURL != "https://a.example" {
		t.Fatalf("第一个 patch 没生效: %+v", base)
	}

	after, err := store.Set(json.RawMessage(`{"workers":2}`))
	if err != nil {
		t.Fatalf("patch 失败: %v", err)
	}

	if after.Workers != 2 {
		t.Errorf("workers = %d, 期望 2", after.Workers)
	}
	if after.BaseURL != "https://a.example" || after.CDNHost != "https://c.example" {
		t.Errorf("只改了 workers 却把线路清掉了: base_url=%q cdn_host=%q", after.BaseURL, after.CDNHost)
	}
	if !after.PDFPerImageBookmark || after.PDFLayout != "single" {
		t.Errorf("未提及的 PDF 配置被改动了: %+v", after)
	}
}

// TestSetTogglePerImageBookmark：新开关要能真正关掉（而不是又被默认值盖回来）。
func TestSetTogglePerImageBookmark(t *testing.T) {
	store := NewStore(Default())

	off, err := store.Set(json.RawMessage(`{"pdf_per_image_bookmark":false}`))
	if err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if off.PDFPerImageBookmark {
		t.Error("显式传 false 应当是 false")
	}

	on, err := store.Set(json.RawMessage(`{"pdf_per_image_bookmark":true}`))
	if err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	if !on.PDFPerImageBookmark {
		t.Error("显式传 true 应当是 true")
	}
}

// TestSetRejectsUnknownFields：写错字段名要报错，不能静默忽略。
//
// 静默忽略的后果是宿主以为配置生效了，实际一直用的是默认值 ——
// 比如把 keep_raw 这类请求参数误写进配置里。
func TestSetRejectsUnknownFields(t *testing.T) {
	store := NewStore(Default())
	if _, err := store.Set(json.RawMessage(`{"keep_raw":true}`)); err == nil {
		t.Error("未知字段应当报错")
	}
	// 报错后不应污染已有配置。
	if got := store.Get(); got.Workers != 4 {
		t.Errorf("失败的 Set 改动了配置: %+v", got)
	}
}

func TestSetEmptyParamsIsError(t *testing.T) {
	store := NewStore(Default())
	if _, err := store.Set(nil); err == nil {
		t.Error("空 params 应当报错")
	}
}

// TestGetReturnsCopy：调用方随便改拿到的配置，不能影响 Store。
func TestGetReturnsCopy(t *testing.T) {
	store := NewStore(Default())

	snap := store.Get()
	snap.Workers = 999
	snap.Secret = "被改了"

	if store.Get().Workers == 999 || store.Get().Secret == "被改了" {
		t.Error("Get 返回的必须是副本，改它不能影响 Store")
	}
}

// ---- Validate ----

func TestValidateNormalizesFields(t *testing.T) {
	cases := []struct {
		name  string
		in    Config
		check func(*testing.T, *Config)
	}{
		{
			name: "pdf_layout 空 → single",
			in:   Config{Secret: "s", PDFLayout: ""},
			check: func(t *testing.T, c *Config) {
				if c.PDFLayout != "single" {
					t.Errorf("pdf_layout = %q", c.PDFLayout)
				}
			},
		},
		{
			name: "负页高 → 0（不限高）",
			in:   Config{Secret: "s", PDFMaxPageHeight: -100},
			check: func(t *testing.T, c *Config) {
				if c.PDFMaxPageHeight != 0 {
					t.Errorf("pdf_max_page_height = %v", c.PDFMaxPageHeight)
				}
			},
		},
		{
			name: "workers 0 → 4",
			in:   Config{Secret: "s", Workers: 0},
			check: func(t *testing.T, c *Config) {
				if c.Workers != 4 {
					t.Errorf("workers = %d", c.Workers)
				}
			},
		},
		{
			name: "base_url 去掉尾斜杠",
			in:   Config{Secret: "s", BaseURL: " https://a.example/ "},
			check: func(t *testing.T, c *Config) {
				if c.BaseURL != "https://a.example" {
					t.Errorf("base_url = %q", c.BaseURL)
				}
			},
		},
	}

	for _, tc := range cases {
		c := tc.in
		if err := c.Validate(); err != nil {
			t.Errorf("%s: 不应报错: %v", tc.name, err)
			continue
		}
		tc.check(t, &c)
	}
}

// TestValidateRejectsUnusableConfig 覆盖两条"必须拦下来"的：
// secret 为空（解不了密，任何命令都会失败）与未知 pdf_layout（拼错了要当场说）。
func TestValidateRejectsUnusableConfig(t *testing.T) {
	c := Config{Secret: ""}
	if err := c.Validate(); err == nil {
		t.Error("secret 为空应当报错，否则所有响应都解不开")
	}

	c = Config{Secret: "s", PDFLayout: "single-page"}
	if err := c.Validate(); err == nil {
		t.Error("未知 pdf_layout 应当报错（拼写错误不该静默退回 single）")
	}
}

// TestPagedLayoutSurvivesValidate：paged 是合法值，别被归一化吃掉。
func TestPagedLayoutSurvivesValidate(t *testing.T) {
	c := Config{Secret: "s", PDFLayout: "paged", PDFMaxPageHeight: 14400}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PDFLayout != "paged" || c.PDFMaxPageHeight != 14400 {
		t.Errorf("paged 配置被改动了: layout=%q maxH=%v", c.PDFLayout, c.PDFMaxPageHeight)
	}
}
