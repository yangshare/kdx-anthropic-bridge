# 多平台上游支持 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将单上游专用桥（仅对接科大）改造为多上游供应商网关，客户端按 proxy key 显式选择上游，配置全面从 `.env` 迁移到 `config.yaml`。

**架构：** 配置层重写为多平台模型（`Config` 含 `Platforms[]` + `Profiles` map）。`server.New()` 为每个平台构造独立的 `*upstream.Client`（注入该平台 profile 的重试/并发/超时），并建立 `proxy_key → 运行态平台` 反查表。鉴权层从请求头提取 token 反查命中平台，未命中返回 401。`rewriter` 改为按 profile 开关决定是否改写 thinking/web_search。`upstream.Client` 与流式过滤逻辑原样复用。

**技术栈：** Go 1.26、`gopkg.in/yaml.v3`（新增）、标准库 `net/http`、现有 `chromedp` 谷歌搜索、TDD（先写测试）。

---

## 待定项决议（已与用户确认）

| 待定项 | 决议 |
|---|---|
| `config.yaml` 路径 | `--config` flag + 默认 `./config.yaml` |
| YAML 未知字段 | 严格模式 `decoder.KnownFields(true)`（拼错字段名启动即 fatal） |
| `.env` 迁移后处理 | 直接删除 `.env` / `.env.example`，`config.example.yaml` 承担示例职责 |

---

## 文件结构

### 创建

| 文件 | 职责 |
|---|---|
| `internal/config/config_test.go` | config 包测试：YAML 解析、Duration、校验、Index |
| `config.example.yaml` | 双平台（kdx + anthropic）示例配置，随仓库提交 |

### 重写

| 文件 | 职责 |
|---|---|
| `internal/config/config.go` | 完全重写：`Config`/`ServerConfig`/`GoogleSearchConfig`/`Platform`/`Profile`/`Duration` 结构 + `Load(path)` + 校验 + 归一化 + `Index()` |
| `cmd/bridge/main.go` | 删 `godotenv`，加 `--config` flag，调 `config.Load(path)`，遍历打印平台 |

### 修改

| 文件 | 职责 |
|---|---|
| `internal/proxy/rewriter.go` | `RewriteRequest` 增加 `RewriteOptions` 参数，按开关短路 |
| `internal/proxy/rewriter_test.go` | 现有用例改传 `RewriteOptions{Thinking:true,WebSearch:true}`；新增 options 关闭/半开用例 |
| `internal/server/server.go` | `New` 遍历平台构造反查表 + 每平台 `*upstream.Client`；`handleAll` 用 `pickPlatform`；删 `authorized`，加 `extractToken` |
| `internal/server/handler_test.go` | `newTestServer` 改用多平台 `Config`；新增多平台路由测试 |
| `internal/proxy/adapter.go` | `NewSearchAdapter` 取 `cfg.GoogleSearch.Proxy`/`.Timeout`（字段名跟随新结构） |
| `go.mod` / `go.sum` | 加 `gopkg.in/yaml.v3`，`go mod tidy` 移除 `godotenv` |
| `.gitignore` | 增加 `/config.yaml`（真实 key 不入库） |
| `README.md` | `.env` 章节替换为 `config.yaml` 章节 + 迁移说明 |
| `docker-compose.yml` | `env_file` 换成挂载 `config.yaml` + `--config` 启动参数 |

### 删除

| 文件 | 原因 |
|---|---|
| `.env` | 配置载体迁移到 YAML |
| `.env.example` | 由 `config.example.yaml` 取代 |

### 不动

| 文件 | 原因 |
|---|---|
| `internal/upstream/client.go` | 重试 + 并发抢窗口逻辑原样复用（规格 4.4） |
| `internal/upstream/client_test.go` | Client 未改，用例不变 |
| `internal/proxy/stream_filter.go` | web_search 响应拦截逻辑不变，触发条件由 `handleAll` 控制 |
| `internal/proxy/stream_filter_test.go` | 过滤器未改 |
| `internal/search/*` | 谷歌搜索实现不变 |
| `internal/anthropic/types.go` | 协议常量不变 |
| `Dockerfile` | 不依赖 `.env`，无需改 |
| `.github/workflows/ci.yml` | 只跑 `gofmt`/`vet`/`build`/`test`，无 `.env` 依赖 |

---

## 关键设计决策

### 1. 运行态平台结构放在 server 包（对规格 4.4 的实现调整）

规格 4.4 建议"给 `config.Platform` 增加运行态字段 `Client *upstream.Client`"。本计划改为在 `server` 包定义运行态结构 `platformRuntime`：

```go
type platformRuntime struct {
    cfg     config.Platform   // 平台配置（name/proxy_key/base_url/api_key/profile 名）
    profile config.Profile    // 预解析的 profile 副本
    client  *upstream.Client  // 该平台专属上游客户端
}
```

**理由：** 让 `config` 包保持纯数据（不 import `internal/upstream`、不感知 `http.Transport`），分层更干净。`pickPlatform` 返回 `*platformRuntime`，可直接取 `client.Forward(...)`，仍只需一张映射表 `byProxyKey map[string]*platformRuntime`，满足规格"无需第二张映射表"的意图。

### 2. Duration 自定义类型

`time.Duration` 是 int64 纳秒，`yaml.v3` 不能直接把 `5s` 解析成它。新增 `config.Duration` 类型实现 `UnmarshalYAML`，内部用 `time.ParseDuration`。`Profile` 的 `RetryInterval`/`HeaderTimeout` 字段用 `Duration`，使用时 `time.Duration(p.HeaderTimeout)` 转换。

### 3. 配置归一化在 `Load` 内完成

YAML 缺省字段会是零值，`Load` 统一归一化：
- `profile.parallel <= 0` → `1`
- `profile.header_timeout <= 0` → `30s`
- `google_search.timeout <= 0` → `15`
- `google_search.limit <= 0` → `5`

运行期不再处理配置默认值。

### 4. 任务 2 的已知中间状态

任务 2 落地配置层与接入层，但 `rewriter` 暂保持单参签名（仍对所有平台全改写）。此时 `official` 平台请求侧也会被改写——这是任务 2 的已知中间缺陷，任务 3 引入 `RewriteOptions` 后修复。任务 2 结束时全项目编译通过、全部现有测试通过。

---

## 任务 1：添加 yaml.v3 依赖

**文件：**
- 修改：`go.mod`、`go.sum`

- [ ] **步骤 1：拉取依赖**

运行：

```bash
go get gopkg.in/yaml.v3@latest
```

预期：`go.mod` 的 `require` 块新增 `gopkg.in/yaml.v3 v3.x.x`，`go.sum` 更新。

- [ ] **步骤 2：验证依赖可用**

运行：

```bash
go build ./...
```

预期：编译通过（此时还未使用 yaml，仅确保依赖下载正常）。

- [ ] **步骤 3：Commit**

```bash
git add go.mod go.sum
git commit -m "chore(deps): 引入 gopkg.in/yaml.v3 用于 YAML 配置"
```

---

## 任务 2：重写配置层 + 接入层适配（项目恢复编译）

此任务把配置模型从单上游切到多平台，并同步适配 `server`/`main`/`adapter`/`handler_test`，结束时全项目编译通过、全部测试通过。`rewriter` 在本任务保持单参签名（任务 3 改造）。

**文件：**
- 重写：`internal/config/config.go`
- 新建：`internal/config/config_test.go`
- 修改：`internal/proxy/adapter.go`
- 修改：`internal/server/server.go`
- 修改：`internal/server/handler_test.go`
- 修改：`cmd/bridge/main.go`

### 步骤 1：编写 config 包测试（先写测试）

- [ ] **步骤 1.1：编写 `internal/config/config_test.go`**

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTempYAML 把 content 写入临时文件，返回路径。t.Cleanup 自动清理。
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return p
}

// fullYAML 规格第 3.1 节完整示例（双平台 + 双 profile）。
const fullYAML = `server:
  host: 0.0.0.0
  port: 8080

google_search:
  proxy: http://127.0.0.1:7890
  timeout: 15
  limit: 5

platforms:
  - name: kdx
    proxy_key: token-kdx-xxxxxx
    base_url: https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic
    api_key: appid:secret
    profile: keding

  - name: anthropic
    proxy_key: token-anthropic-yyyyyy
    base_url: https://api.anthropic.com
    api_key: sk-ant-xyz
    profile: official

profiles:
  keding:
    rewrite_thinking: true
    rewrite_web_search: true
    max_retries: 10
    retry_interval: 5s
    header_timeout: 30s
    parallel: 1

  official:
    rewrite_thinking: false
    rewrite_web_search: false
    max_retries: 0
    header_timeout: 60s
    parallel: 1
`

func TestLoad_fullExample(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, fullYAML))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("server.host = %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("server.port = %d", cfg.Server.Port)
	}
	if cfg.GoogleSearch.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("google_search.proxy = %q", cfg.GoogleSearch.Proxy)
	}
	if cfg.GoogleSearch.Timeout != 15 {
		t.Errorf("google_search.timeout = %d", cfg.GoogleSearch.Timeout)
	}
	if cfg.GoogleSearch.Limit != 5 {
		t.Errorf("google_search.limit = %d", cfg.GoogleSearch.Limit)
	}

	if len(cfg.Platforms) != 2 {
		t.Fatalf("platforms count = %d, want 2", len(cfg.Platforms))
	}
	kdx := cfg.Platforms[0]
	if kdx.Name != "kdx" || kdx.ProxyKey != "token-kdx-xxxxxx" || kdx.Profile != "keding" {
		t.Errorf("kdx platform mismatch: %+v", kdx)
	}
	if kdx.BaseURL != "https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic" {
		t.Errorf("kdx base_url = %q", kdx.BaseURL)
	}
	if kdx.APIKey != "appid:secret" {
		t.Errorf("kdx api_key = %q", kdx.APIKey)
	}

	keding := cfg.Profiles["keding"]
	if !keding.RewriteThinking || !keding.RewriteWebSearch {
		t.Errorf("keding rewrite flags wrong: %+v", keding)
	}
	if keding.MaxRetries != 10 {
		t.Errorf("keding max_retries = %d", keding.MaxRetries)
	}
	// Duration 字符串解析
	if time.Duration(keding.RetryInterval) != 5*time.Second {
		t.Errorf("keding retry_interval = %v, want 5s", keding.RetryInterval)
	}
	if time.Duration(keding.HeaderTimeout) != 30*time.Second {
		t.Errorf("keding header_timeout = %v, want 30s", keding.HeaderTimeout)
	}
	if keding.Parallel != 1 {
		t.Errorf("keding parallel = %d", keding.Parallel)
	}
}

func TestLoad_durationParsed(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p:
    retry_interval: 1m30s
    header_timeout: 2s
`
	cfg, err := Load(writeTempYAML(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Profiles["p"]
	if time.Duration(p.RetryInterval) != 90*time.Second {
		t.Errorf("retry_interval = %v, want 90s", p.RetryInterval)
	}
	if time.Duration(p.HeaderTimeout) != 2*time.Second {
		t.Errorf("header_timeout = %v, want 2s", p.HeaderTimeout)
	}
}

func TestLoad_invalidDuration(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p: {header_timeout: not-a-duration}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("invalid duration should error")
	}
}

func TestLoad_fileNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("missing file should error")
	}
}

func TestLoad_emptyPlatforms(t *testing.T) {
	yaml := `server: {port: 8080}
platforms: []
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("empty platforms should error")
	}
}

func TestLoad_duplicateProxyKey(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: dup, base_url: http://x, api_key: ak, profile: p}
  - {name: b, proxy_key: dup, base_url: http://y, api_key: ak, profile: p}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("duplicate proxy_key should error")
	}
}

func TestLoad_unknownProfileRef(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: ghost}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("unknown profile reference should error")
	}
}

func TestLoad_invalidPort(t *testing.T) {
	yaml := `server: {port: 99999}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("invalid port should error")
	}
}

func TestLoad_unknownFieldRejected(t *testing.T) {
	// KnownFields(true):拼错字段名应启动报错
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p, rewite_thinking: true}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("unknown field should error in strict mode")
	}
}

func TestLoad_normalizeDefaults(t *testing.T) {
	// parallel/header_timeout/google_search 缺省时归一化
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p: {}
`
	cfg, err := Load(writeTempYAML(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Profiles["p"]
	if p.Parallel != 1 {
		t.Errorf("parallel default = %d, want 1", p.Parallel)
	}
	if time.Duration(p.HeaderTimeout) != 30*time.Second {
		t.Errorf("header_timeout default = %v, want 30s", p.HeaderTimeout)
	}
	if cfg.GoogleSearch.Timeout != 15 {
		t.Errorf("google_search.timeout default = %d, want 15", cfg.GoogleSearch.Timeout)
	}
	if cfg.GoogleSearch.Limit != 5 {
		t.Errorf("google_search.limit default = %d, want 5", cfg.GoogleSearch.Limit)
	}
}

func TestIndex_proxyKeyLookup(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, fullYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	idx := cfg.Index()
	if len(idx) != 2 {
		t.Fatalf("index size = %d, want 2", len(idx))
	}
	kdx, ok := idx["token-kdx-xxxxxx"]
	if !ok || kdx.Name != "kdx" {
		t.Errorf("kdx lookup failed: %+v", kdx)
	}
	ant, ok := idx["token-anthropic-yyyyyy"]
	if !ok || ant.Name != "anthropic" {
		t.Errorf("anthropic lookup failed: %+v", ant)
	}
	if _, ok := idx["unknown-key"]; ok {
		t.Error("unknown key should not resolve")
	}
}
```

- [ ] **步骤 1.2：运行测试验证失败**

运行：

```bash
go test ./internal/config/ -v
```

预期：FAIL，报错 `undefined: Load`（新签名 `Load(path)` 还不存在，旧 `Load()` 无参）。

### 步骤 2：重写 `internal/config/config.go`

- [ ] **步骤 2.1：用以下内容完整替换 `internal/config/config.go`**

```go
// Package config 加载代理运行配置(config.yaml)。
//
// 配置模型:多个上游平台(platforms),每个绑定一个 proxy_key 作为路由键;
// 改写规则与重试/并发参数由 profile 模板封装,平台引用 profile。
// 启动时完成全部校验与归一化,运行期不再处理配置错误。
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 代理运行配置。
type Config struct {
	Server       ServerConfig       `yaml:"server"`
	GoogleSearch GoogleSearchConfig `yaml:"google_search"`
	Platforms    []Platform         `yaml:"platforms"`
	Profiles     map[string]Profile `yaml:"profiles"`
}

// ServerConfig 网关自身监听配置。
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// GoogleSearchConfig 谷歌搜索(WebSearch 响应侧拦截用)。
type GoogleSearchConfig struct {
	Proxy   string `yaml:"proxy"`
	Timeout int    `yaml:"timeout"`
	Limit   int    `yaml:"limit"`
}

// Platform 上游实例。每个实例绑定一个全局唯一的 proxy_key 作为路由键。
type Platform struct {
	Name     string `yaml:"name"`
	ProxyKey string `yaml:"proxy_key"`
	BaseURL  string `yaml:"base_url"`
	APIKey   string `yaml:"api_key"`
	Profile  string `yaml:"profile"` // 引用 Profiles 的 key
}

// Profile 改写模板:封装该平台的协议适配开关 + 重试/并发/超时参数。
type Profile struct {
	RewriteThinking  bool     `yaml:"rewrite_thinking"`
	RewriteWebSearch bool     `yaml:"rewrite_web_search"`
	MaxRetries       int      `yaml:"max_retries"`
	RetryInterval    Duration `yaml:"retry_interval"`
	HeaderTimeout    Duration `yaml:"header_timeout"`
	Parallel         int      `yaml:"parallel"`
}

// Duration 包装 time.Duration,支持从 YAML 字符串解析("5s"/"30s"/"1m30s")。
type Duration time.Duration

// UnmarshalYAML 把 YAML 字符串解析为 time.Duration。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Load 从 path 读取并解析 config.yaml,执行校验与归一化。
// 任何配置错误都返回 error,由调用方 log.Fatal。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	// 严格模式:未知字段(拼写错误)报错,避免配置不生效而不察觉
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.normalize()
	return &cfg, nil
}

// validate 启动时校验,失败返回携带具体冲突项的 error。
func (c *Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range [1,65535]", c.Server.Port)
	}
	if len(c.Platforms) == 0 {
		return fmt.Errorf("config: platforms is empty (at least one upstream required)")
	}

	// proxy_key 全局唯一 + 必填 + base_url/api_key/profile 必填
	seen := make(map[string]bool, len(c.Platforms))
	for i, p := range c.Platforms {
		if p.ProxyKey == "" {
			return fmt.Errorf("config: platforms[%d].proxy_key is empty", i)
		}
		if seen[p.ProxyKey] {
			return fmt.Errorf("config: duplicate proxy_key %q", p.ProxyKey)
		}
		seen[p.ProxyKey] = true
		if p.BaseURL == "" {
			return fmt.Errorf("config: platforms[%d] (%s).base_url is empty", i, p.Name)
		}
		if p.APIKey == "" {
			return fmt.Errorf("config: platforms[%d] (%s).api_key is empty", i, p.Name)
		}
		if p.Profile == "" {
			return fmt.Errorf("config: platforms[%d] (%s).profile is empty", i, p.Name)
		}
		if _, ok := c.Profiles[p.Profile]; !ok {
			return fmt.Errorf("config: platforms[%d] (%s) references unknown profile %q",
				i, p.Name, p.Profile)
		}
	}
	return nil
}

// normalize 归一化缺省字段为安全默认值。
func (c *Config) normalize() {
	if c.GoogleSearch.Timeout <= 0 {
		c.GoogleSearch.Timeout = 15
	}
	if c.GoogleSearch.Limit <= 0 {
		c.GoogleSearch.Limit = 5
	}
	for name, p := range c.Profiles {
		if p.Parallel <= 0 {
			p.Parallel = 1
		}
		if p.HeaderTimeout <= 0 {
			p.HeaderTimeout = Duration(30 * time.Second)
		}
		c.Profiles[name] = p
	}
}

// Index 构造 proxy_key -> *Platform 反查表,供鉴权层 O(1) 查找。
func (c *Config) Index() map[string]*Platform {
	idx := make(map[string]*Platform, len(c.Platforms))
	for i := range c.Platforms {
		p := &c.Platforms[i]
		idx[p.ProxyKey] = p
	}
	return idx
}
```

- [ ] **步骤 2.2：运行 config 包测试验证通过**

运行：

```bash
go test ./internal/config/ -v
```

预期：PASS（全部 config 用例通过）。

> 注意：此时 `go build ./...` 仍失败，因为 `server`/`cmd/bridge` 还引用旧 `Config` 字段。步骤 3-6 修复。

### 步骤 3：适配 `internal/proxy/adapter.go`

- [ ] **步骤 3.1：修改 `NewSearchAdapter` 取新字段**

把 `internal/proxy/adapter.go` 的 `NewSearchAdapter` 函数体改为：

```go
// NewSearchAdapter 从配置构造搜索执行器。
func NewSearchAdapter(cfg *config.Config) *WebSearchExecutorAdapter {
	timeout := time.Duration(cfg.GoogleSearch.Timeout) * time.Second
	return &WebSearchExecutorAdapter{
		google: &search.GoogleSearcher{
			Proxy:   cfg.GoogleSearch.Proxy,
			Timeout: timeout,
		},
	}
}
```

（仅改 `cfg.GoogleSearchProxy` → `cfg.GoogleSearch.Proxy`、`cfg.GoogleSearchTimeout` → `cfg.GoogleSearch.Timeout`，import 不变。）

### 步骤 4：改造 `internal/server/server.go`

- [ ] **步骤 4.1：用以下内容完整替换 `internal/server/server.go`**

```go
// Package server 实现代理 HTTP 服务。
//
// 接入层职责:接收 Claude Code 请求、按 proxy key 路由到对应上游平台、
// 调用业务层(proxy.RewriteRequest)按平台 profile 改写、
// 调用基础层(upstream.Client)转发、流式回传响应。不写业务判断。
package server

import (
	"io"
	"log"
	"net/http"
	"time"

	"github.com/godkey/kdx-anthropic-bridge/internal/anthropic"
	"github.com/godkey/kdx-anthropic-bridge/internal/config"
	"github.com/godkey/kdx-anthropic-bridge/internal/proxy"
	"github.com/godkey/kdx-anthropic-bridge/internal/upstream"
)

// platformRuntime 平台运行态:配置 + 预解析 profile + 该平台专属上游客户端。
// 鉴权层 pickPlatform 返回 *platformRuntime,handleAll 直接取 client.Forward。
type platformRuntime struct {
	cfg     config.Platform
	profile config.Profile
	client  *upstream.Client
}

// Server 代理 HTTP 服务。
type Server struct {
	cfg        *config.Config
	byProxyKey map[string]*platformRuntime
	rewriter   func([]byte) (*proxy.RewriteResult, error)
	// searcher 谷歌搜索执行器,响应侧拦截 web_search tool_use 时用。
	// 为 nil 时不做响应过滤(web_search 走原样透传)。
	searcher    *proxy.WebSearchExecutorAdapter
	googleLimit int
}

// New 构造 Server:为每个平台构造独立 *upstream.Client(注入该平台 profile
// 的 HeaderTimeout 作 transport ResponseHeaderTimeout、MaxRetries、RetryInterval、
// Parallel,以及平台 BaseURL/APIKey),建立 proxy_key 反查表。
func New(cfg *config.Config) *Server {
	byProxyKey := make(map[string]*platformRuntime, len(cfg.Platforms))
	for i := range cfg.Platforms {
		pc := &cfg.Platforms[i]
		prof := cfg.Profiles[pc.Profile] // 校验已保证存在

		// 每平台独立 transport:ResponseHeaderTimeout 随 profile 变化,平台间互不干扰。
		// 用 ResponseHeaderTimeout 而非 http.Client.Timeout:
		// 只限"等上游响应头"的时间,一旦开始流式返回,传输不限总时长
		// (长文档/长思考可慢慢流,不会被掐断)。
		transport := &http.Transport{
			ResponseHeaderTimeout: time.Duration(prof.HeaderTimeout),
		}
		client := &upstream.Client{
			BaseURL:       pc.BaseURL,
			APIKey:        pc.APIKey,
			HTTP:          &http.Client{Transport: transport},
			MaxRetries:    prof.MaxRetries,
			RetryInterval: time.Duration(prof.RetryInterval),
			Parallel:      prof.Parallel,
		}
		byProxyKey[pc.ProxyKey] = &platformRuntime{
			cfg:     *pc,
			profile: prof,
			client:  client,
		}
	}

	s := &Server{
		cfg:         cfg,
		byProxyKey:  byProxyKey,
		rewriter:    proxy.RewriteRequest,
		googleLimit: cfg.GoogleSearch.Limit,
	}
	// 配了谷歌搜索代理才启用 web_search 响应过滤
	if cfg.GoogleSearch.Proxy != "" {
		s.searcher = proxy.NewSearchAdapter(cfg)
	}
	return s
}

// Routes 返回配置好路由的 http.Handler。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleAll)
	return mux
}

// handleAll 统一入口:按 proxy key 路由到平台,鉴权 + 改写 + 转发 + 流式回传。
func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	p := s.pickPlatform(r)
	if p == nil {
		writeError(w, http.StatusUnauthorized, "invalid or unknown proxy key")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body failed")
		return
	}
	hasWebSearch := false

	// 仅 /v1/messages 改写请求体,其他路径原样透传
	if r.URL.Path == anthropic.PathMessages && r.Method == http.MethodPost {
		result, err := s.rewriter(body) // 任务 3 改为按 profile 传 RewriteOptions
		if err != nil {
			writeError(w, http.StatusBadRequest, "rewrite request body failed")
			return
		}
		body = result.Body
		hasWebSearch = result.HasWebSearch
	}

	// 透传路径(含 query)。body 以 []byte 传入,支持上游重试时重放
	tStart := time.Now()
	resp, err := p.client.Forward(r.Method, r.URL.RequestURI(), body, r.Header)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream forward failed")
		log.Printf("upstream error after %s: %v", time.Since(tStart), err)
		return
	}
	defer resp.Body.Close()
	headerWait := time.Since(tStart)

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 仅当 profile 开了 rewrite_web_search、请求含 web_search、且配了搜索器,
	// 才走流式拦截;否则原样透传
	if hasWebSearch && p.profile.RewriteWebSearch && s.searcher != nil {
		filter := proxy.NewStreamFilter(s.searcher, s.googleLimit)
		if err := filter.FilterStream(r.Context(), w, resp.Body); err != nil {
			log.Printf("stream filter error: %v", err)
		}
		return
	}

	// 其他:流式透传
	n, _ := io.Copy(w, resp.Body)
	log.Printf("done platform=%s path=%s status=%d header_wait=%s stream=%s total=%s bytes=%d",
		p.cfg.Name, r.URL.Path, resp.StatusCode, headerWait, time.Since(tStart)-headerWait,
		time.Since(tStart), n)
}

// pickPlatform 从请求头提取 token,反查命中则返回该平台运行态;未命中返回 nil。
func (s *Server) pickPlatform(r *http.Request) *platformRuntime {
	token := extractToken(r)
	if token == "" {
		return nil
	}
	return s.byProxyKey[token]
}

// extractToken 从 Authorization: Bearer <t> 或 x-api-key: <t> 提取 token。
func extractToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			return auth[len(prefix):]
		}
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	return ""
}
```

> **说明：** `rewriter` 字段在本任务保持单参签名 `func([]byte) (...)`，`handleAll` 调用 `s.rewriter(body)`。任务 3 将签名改为双参并传入 profile 的 `RewriteOptions`。

### 步骤 5：改造 `cmd/bridge/main.go`

- [ ] **步骤 5.1：用以下内容完整替换 `cmd/bridge/main.go`**

```go
// Package main 是 kdx-anthropic-bridge 代理入口。
//
// 启动后读取 config.yaml(--config 指定,默认工作目录下 config.yaml),
// 监听 server.host:server.port,接收 Claude Code 的 Anthropic 协议请求,
// 按 proxy key 路由到对应上游平台,响应流式透传。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/godkey/kdx-anthropic-bridge/internal/config"
	"github.com/godkey/kdx-anthropic-bridge/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	srv := server.New(cfg)
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Routes(),
	}

	// 优雅退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		httpSrv.Close()
	}()

	log.Printf("kdx-anthropic-bridge listening on %s", addr)
	for i := range cfg.Platforms {
		p := &cfg.Platforms[i]
		log.Printf("platform %s -> %s (profile=%s)", p.Name, p.BaseURL, p.Profile)
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
```

### 步骤 6：更新 `internal/server/handler_test.go`

- [ ] **步骤 6.1：替换 `newTestServer` 辅助函数**

把 `internal/server/handler_test.go` 顶部的 `newTestServer` 函数（从 `func newTestServer` 到其闭合 `}`）替换为：

```go
// newTestServer 起一个带假上游的测试 Server(单平台,profile 全开)。
// upstreamHandler 处理假上游请求,返回模拟响应。
func newTestServer(t *testing.T, upstreamHandler http.HandlerFunc) *Server {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0}, // 不实际监听,用 httptest
		GoogleSearch: config.GoogleSearchConfig{
			Timeout: 15,
			Limit:   5,
		},
		Platforms: []config.Platform{
			{
				Name:     "test",
				ProxyKey: "test-proxy-key",
				BaseURL:  up.URL,
				APIKey:   "fake-upstream-key",
				Profile:  "default",
			},
		},
		Profiles: map[string]config.Profile{
			"default": {
				RewriteThinking:  true,
				RewriteWebSearch: true,
				HeaderTimeout:    config.Duration(30 * time.Second),
				Parallel:         1,
			},
		},
	}
	s := New(cfg)
	// 注入假上游 client(覆盖默认 transport,用 httptest 的)
	s.byProxyKey["test-proxy-key"].client.HTTP = up.Client()
	return s
}
```

- [ ] **步骤 6.2：更新 handler_test.go 的 import 块**

把 `internal/server/handler_test.go` 的 import 块改为（新增 `time`）：

```go
import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/godkey/kdx-anthropic-bridge/internal/config"
)
```

- [ ] **步骤 6.3：验证现有 handler 测试语义不变**

现有测试（`TestHandler_messagesRewritesThinking` 等）调用 `doProxy(s, ..., "test-proxy-key")` 命中 `test` 平台，其 `default` profile 全开，`rewriter` 单参全改写——与改造前行为一致。这些用例**无需改动函数体**，仅依赖更新后的 `newTestServer`。

### 步骤 7：全量编译与测试

- [ ] **步骤 7.1：编译全项目**

运行：

```bash
go build ./...
```

预期：编译通过，无报错。

- [ ] **步骤 7.2：运行全部测试**

运行：

```bash
go test ./... -v
```

预期：PASS。`config` / `proxy` / `server` / `upstream` / `search` 全部用例通过。

> 已知中间状态：此时 `rewriter` 仍单参全改写，多平台改写差异在任务 3 完成。

- [ ] **步骤 7.3：Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/proxy/adapter.go internal/server/server.go internal/server/handler_test.go cmd/bridge/main.go
git commit -m "feat(config): 配置层迁移到多平台 YAML,按 proxy key 路由"
```

---

## 任务 3：rewriter 按 profile 改写 + 多平台路由

`rewriter` 增加 `RewriteOptions`，按开关决定是否改写 thinking/web_search；`handleAll` 传入命中平台的 profile 开关；新增多平台路由测试。

**文件：**
- 修改：`internal/proxy/rewriter.go`
- 修改：`internal/proxy/rewriter_test.go`
- 修改：`internal/server/server.go`
- 修改：`internal/server/handler_test.go`

### 步骤 1：扩展 rewriter 测试

- [ ] **步骤 1.1：把现有测试里的 `RewriteRequest` 调用改为双参签名**

现有测试全部调用 `RewriteRequest(in)`（单参）。把它们改为 `RewriteRequest(in, RewriteOptions{Thinking: true, WebSearch: true})`，保持原语义（原默认全改写）。

需要修改的调用点（每处把 `RewriteRequest(in)` → `RewriteRequest(in, RewriteOptions{Thinking: true, WebSearch: true})`，`RewriteRequest([]byte{})` → `RewriteRequest([]byte{}, RewriteOptions{Thinking: true, WebSearch: true})`，`RewriteRequest(in)` 在 `TestRewriteRequest_invalidJSON_error` 同理）：

- `TestRewriteRequest_adaptiveToEnabled`
- `TestRewriteRequest_alreadyEnabled_unchanged`
- `TestRewriteRequest_noThinking_unchanged`
- `TestRewriteRequest_nonAdaptiveType_unchanged`
- `TestRewriteRequest_thinkingNotMap_unchanged`
- `TestRewriteRequest_emptyBody_unchanged`
- `TestRewriteRequest_invalidJSON_error`
- `TestRewriteRequest_preservesUnknownFields`
- `TestRewriteRequest_webSearchToolRewritten`
- `TestRewriteRequest_webSearchMixedWithOtherTools`
- `TestRewriteRequest_noWebSearch_hasWebSearchFalse`

- [ ] **步骤 1.2：在 `rewriter_test.go` 末尾追加 options 开关用例**

```go
// ===== RewriteOptions 开关测试 =====

func TestRewriteRequest_optionsDisabled_passthrough(t *testing.T) {
	// 两开关全关(如 official profile):body 原样返回,不序列化,HasWebSearch=false
	in := []byte(`{"thinking":{"type":"adaptive"},"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	r, err := RewriteRequest(in, RewriteOptions{Thinking: false, WebSearch: false})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(r.Body) != string(in) {
		t.Errorf("options disabled should return original body\ngot:  %s\nwant: %s", r.Body, in)
	}
	if r.HasWebSearch {
		t.Errorf("HasWebSearch should be false when WebSearch option off")
	}
}

func TestRewriteRequest_onlyThinking(t *testing.T) {
	// 只开 thinking:adaptive 被改写,web_search 工具原样保留
	in := []byte(`{"thinking":{"type":"adaptive"},"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	r, err := RewriteRequest(in, RewriteOptions{Thinking: true, WebSearch: false})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if r.HasWebSearch {
		t.Errorf("HasWebSearch should be false when WebSearch option off")
	}
	m := parseHelper(t, r.Body)
	if th, _ := m["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Errorf("thinking not rewritten: %v", m["thinking"])
	}
	tools, _ := m["tools"].([]any)
	if tools[0].(map[string]any)["type"] != "web_search_20250305" {
		t.Errorf("web_search tool should be preserved when WebSearch option off: %v", tools[0])
	}
}

func TestRewriteRequest_onlyWebSearch(t *testing.T) {
	// 只开 web_search:thinking 原样保留,web_search 被改写
	in := []byte(`{"thinking":{"type":"adaptive"},"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	r, err := RewriteRequest(in, RewriteOptions{Thinking: false, WebSearch: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !r.HasWebSearch {
		t.Errorf("HasWebSearch should be true")
	}
	m := parseHelper(t, r.Body)
	if th, _ := m["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking should be preserved when Thinking option off: %v", m["thinking"])
	}
	tools, _ := m["tools"].([]any)
	if tools[0].(map[string]any)["type"] == "web_search_20250305" {
		t.Errorf("web_search tool should be rewritten when WebSearch option on")
	}
}
```

- [ ] **步骤 1.3：运行测试验证失败**

运行：

```bash
go test ./internal/proxy/ -run TestRewriteRequest -v
```

预期：FAIL，报错 `wrong number of args for RewriteRequest` 或 `undefined: RewriteOptions`。

### 步骤 2：改造 `internal/proxy/rewriter.go`

- [ ] **步骤 2.1：修改 `RewriteResult` 之后的 `RewriteRequest` 函数**

在 `internal/proxy/rewriter.go` 中，找到现有 `RewriteRequest` 函数（从 `func RewriteRequest(body []byte) (*RewriteResult, error) {` 到其闭合 `}`），整体替换为：

```go
// RewriteOptions 控制 RewriteRequest 按平台 profile 改写哪些字段。
type RewriteOptions struct {
	Thinking  bool // profile.RewriteThinking
	WebSearch bool // profile.RewriteWebSearch
}

// RewriteRequest 改写 Anthropic /v1/messages 请求体,按 opt 开关决定改写哪些字段。
//
// 改写规则(其他字段一律透传):
//   - opt.Thinking && thinking.type == "adaptive"  ->  {"type":"enabled"}
//   - opt.WebSearch && tools 里的 web_search_20250305  ->  普通 function tool(带 query input_schema)
//
// 没有需要改的字段时返回原始 body,避免重新序列化改变字节顺序。
// 两开关全关(如 official profile)时等同透传。
//
// HasWebSearch 仅在 opt.WebSearch 开启且请求含 web_search 工具时为 true
// (响应侧拦截据此 + profile.RewriteWebSearch 共同决定)。
func RewriteRequest(body []byte, opt RewriteOptions) (*RewriteResult, error) {
	if len(body) == 0 {
		return &RewriteResult{Body: body}, nil
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("rewriter: parse request body: %w", err)
	}

	changed := false
	if opt.Thinking {
		if rewriteThinking(req) {
			changed = true
		}
	}
	hasWS := false
	if opt.WebSearch {
		hasWS = rewriteWebSearchTools(req)
	}

	if !changed && !hasWS {
		return &RewriteResult{Body: body, HasWebSearch: hasWS}, nil
	}

	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("rewriter: marshal request body: %w", err)
	}
	return &RewriteResult{Body: out, HasWebSearch: hasWS}, nil
}
```

`rewriteThinking` / `rewriteWebSearchTools` / `webSearchFunctionTool` 三个内部函数**保持不变**。

- [ ] **步骤 2.2：运行 rewriter 测试验证通过**

运行：

```bash
go test ./internal/proxy/ -run TestRewriteRequest -v
```

预期：PASS。

> 此时 `go build ./...` 失败，因为 `server.go` 的 `rewriter` 字段类型与 `handleAll` 调用还是单参。步骤 3 修复。

### 步骤 3：适配 `internal/server/server.go`

- [ ] **步骤 3.1：修改 `Server.rewriter` 字段类型**

在 `internal/server/server.go` 的 `Server` 结构体中，把：

```go
	rewriter   func([]byte) (*proxy.RewriteResult, error)
```

改为：

```go
	rewriter func([]byte, proxy.RewriteOptions) (*proxy.RewriteResult, error)
```

- [ ] **步骤 3.2：修改 `handleAll` 中的 rewriter 调用**

在 `handleAll` 中，把：

```go
		result, err := s.rewriter(body) // 任务 3 改为按 profile 传 RewriteOptions
```

改为：

```go
		result, err := s.rewriter(body, proxy.RewriteOptions{
			Thinking:  p.profile.RewriteThinking,
			WebSearch: p.profile.RewriteWebSearch,
		})
```

- [ ] **步骤 3.3：编译全项目**

运行：

```bash
go build ./...
```

预期：编译通过。

- [ ] **步骤 3.4：运行全部测试**

运行：

```bash
go test ./... -v
```

预期：PASS。

### 步骤 4：新增多平台路由测试

- [ ] **步骤 4.1：在 `internal/server/handler_test.go` 末尾追加多平台测试**

```go
// TestHandler_multiPlatformRouting 两个平台(kdx 改写 / anthropic 透传)+ 未知 key 401。
// 验证不同 proxy key 路由到不同上游,且改写按 profile 差异生效。
func TestHandler_multiPlatformRouting(t *testing.T) {
	var kdxBody, anthropicBody []byte
	kdxUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kdxBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\ndata: {}\n")
	}))
	t.Cleanup(kdxUp.Close)
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\ndata: {}\n")
	}))
	t.Cleanup(anthropicUp.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Platforms: []config.Platform{
			{Name: "kdx", ProxyKey: "token-kdx", BaseURL: kdxUp.URL, APIKey: "kdx-key", Profile: "keding"},
			{Name: "anthropic", ProxyKey: "token-anthropic", BaseURL: anthropicUp.URL, APIKey: "ant-key", Profile: "official"},
		},
		Profiles: map[string]config.Profile{
			"keding":   {RewriteThinking: true, RewriteWebSearch: true, HeaderTimeout: config.Duration(30 * time.Second)},
			"official": {RewriteThinking: false, RewriteWebSearch: false, HeaderTimeout: config.Duration(60 * time.Second)},
		},
	}
	s := New(cfg)
	s.byProxyKey["token-kdx"].client.HTTP = kdxUp.Client()
	s.byProxyKey["token-anthropic"].client.HTTP = anthropicUp.Client()

	body := `{"thinking":{"type":"adaptive"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`

	// kdx 平台:thinking + web_search 都改写
	doProxy(s, "POST", "/v1/messages", body, "token-kdx")
	if !strings.Contains(string(kdxBody), `"type":"enabled"`) {
		t.Errorf("kdx should rewrite thinking\ngot: %s", kdxBody)
	}
	if strings.Contains(string(kdxBody), "adaptive") {
		t.Errorf("kdx should remove adaptive\ngot: %s", kdxBody)
	}
	if strings.Contains(string(kdxBody), "web_search_20250305") {
		t.Errorf("kdx should rewrite web_search tool\ngot: %s", kdxBody)
	}

	// anthropic 平台:全透传
	doProxy(s, "POST", "/v1/messages", body, "token-anthropic")
	if !strings.Contains(string(anthropicBody), "adaptive") {
		t.Errorf("anthropic should pass thinking through\ngot: %s", anthropicBody)
	}
	if !strings.Contains(string(anthropicBody), "web_search_20250305") {
		t.Errorf("anthropic should keep web_search_20250305\ngot: %s", anthropicBody)
	}

	// 未知 key:401
	rec := doProxy(s, "POST", "/v1/messages", body, "token-unknown")
	if rec.Code != 401 {
		t.Errorf("unknown key status = %d, want 401", rec.Code)
	}
}

// TestHandler_webSearchInterceptOnlyWhenProfileEnabled kdx 开了 web_search 改写,
// 请求带 web_search 工具时响应走拦截路径(此处 searcher 为 nil,验证不 panic 即可)。
func TestHandler_webSearchInterceptOnlyWhenProfileEnabled(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\ndata: {}\n")
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Platforms: []config.Platform{
			{Name: "official", ProxyKey: "tok", BaseURL: up.URL, APIKey: "k", Profile: "official"},
		},
		Profiles: map[string]config.Profile{
			"official": {RewriteWebSearch: false, HeaderTimeout: config.Duration(30 * time.Second)},
		},
	}
	s := New(cfg)
	s.byProxyKey["tok"].client.HTTP = up.Client()

	// official profile 关了 web_search 改写:即使请求带 web_search_20250305,
	// 也不改写、不触发响应拦截(searcher 也为 nil),原样透传,不 panic。
	body := `{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`
	rec := doProxy(s, "POST", "/v1/messages", body, "tok")
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
```

- [ ] **步骤 4.2：运行测试验证通过**

运行：

```bash
go test ./internal/server/ -v
```

预期：PASS（含新增多平台用例）。

- [ ] **步骤 4.3：Commit**

```bash
git add internal/proxy/rewriter.go internal/proxy/rewriter_test.go internal/server/server.go internal/server/handler_test.go
git commit -m "feat(rewriter): 按 profile 开关改写 thinking/web_search,多平台路由差异化"
```

---

## 任务 4：config.example.yaml 与 .gitignore

**文件：**
- 创建：`config.example.yaml`
- 修改：`.gitignore`

- [ ] **步骤 1：创建 `config.example.yaml`**

写入 `config.example.yaml`（双平台示例，含完整注释）：

```yaml
# kdx-anthropic-bridge 配置示例
# 用法:cp config.example.yaml config.yaml,填入真实 key 后启动。
# 真实 config.yaml 不入库(已在 .gitignore),本文件随仓库提交作为模板。

# 网关自身
server:
  host: 0.0.0.0
  port: 8080

# 谷歌搜索(WebSearch 功能;proxy 留空则禁用响应侧 web_search 拦截)
google_search:
  proxy: http://127.0.0.1:7890   # 谷歌直连会超时,必填一个能访问谷歌的代理
  timeout: 15                    # 秒
  limit: 5                       # 每次搜索返回结果数

# 上游实例列表:每个实例绑定一个 proxy_key(客户端用它当 ANTHROPIC_AUTH_TOKEN 触发该上游)
platforms:
  - name: kdx
    proxy_key: token-kdx-xxxxxx          # 自己设随机串,Claude Code 用它鉴权
    base_url: https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic
    api_key: appid:secret                # 科大上游真实 key
    profile: keding                      # 引用 profiles.keding

  - name: anthropic
    proxy_key: token-anthropic-yyyyyy
    base_url: https://api.anthropic.com
    api_key: sk-ant-...                  # 官方上游真实 key
    profile: official

# 改写模板:封装该平台的全部适配规则 + 重试/并发参数
profiles:
  keding:
    rewrite_thinking: true       # thinking.type=adaptive -> enabled(科大需要)
    rewrite_web_search: true     # web_search_20250305 -> function tool(科大需要)
    max_retries: 10              # 502/503/429 重试次数(不含首次)
    retry_interval: 5s           # 重试间隔,>0 才 sleep
    header_timeout: 30s          # 等上游响应头超时;流式开始后不再计时
    parallel: 1                  # 单次 attempt 并发抢窗口路数,<=1 串行

  official:
    rewrite_thinking: false      # 官方原生支持,不改写
    rewrite_web_search: false
    max_retries: 0               # 不重试
    header_timeout: 60s
    parallel: 1
```

- [ ] **步骤 2：更新 `.gitignore`**

在 `.gitignore` 的 `# 环境` 段，把：

```
# 环境
.env
```

改为：

```
# 配置(含真实 key,不入库;config.example.yaml 是模板,保留)
config.yaml
```

- [ ] **步骤 3：验证 config.example.yaml 可被解析**

运行（用 PowerShell）：

```powershell
go run ./cmd/bridge --config config.example.yaml
```

预期：进程启动，打印监听地址与两个平台日志（随后可用 Ctrl+C 停止；若端口被占用会报 `server error`，属正常，重点看无 `config load failed` / `unknown field` 报错）。

> 若 `config.example.yaml` 里的 `sk-ant-...` 等占位 key 触发上游错误不影响本步骤——本步骤只验证 YAML 能被严格模式解析、校验通过。

- [ ] **步骤 4：Commit**

```bash
git add config.example.yaml .gitignore
git commit -m "docs(config): 新增 config.example.yaml 双平台示例,gitignore 真实配置"
```

---

## 任务 5：废弃 .env、更新文档与 Docker

**文件：**
- 删除：`.env`、`.env.example`
- 修改：`go.mod`、`go.sum`（移除 godotenv）
- 修改：`README.md`
- 修改：`docker-compose.yml`

### 步骤 1：删除 .env 相关文件

- [ ] **步骤 1.1：删除文件**

运行：

```bash
git rm .env .env.example
```

预期：两个文件从仓库删除并暂存。

### 步骤 2：移除 godotenv 依赖

- [ ] **步骤 2.1：整理依赖**

运行：

```bash
go mod tidy
```

预期：`go.mod` 中 `github.com/joho/godotenv` 行被移除（main.go 不再 import 它），`go.sum` 同步更新。

- [ ] **步骤 2.2：验证编译**

运行：

```bash
go build ./...
```

预期：编译通过。

### 步骤 3：更新 docker-compose.yml

- [ ] **步骤 3.1：替换 `docker-compose.yml` 全文**

```yaml
services:
  kdx-anthropic-bridge:
    build: .
    volumes:
      - ./config.yaml:/config.yaml:ro
    command: ["--config", "/config.yaml"]
    ports:
      - "8080:8080"
    restart: unless-stopped
```

> 端口映射固定 `8080:8080`。若 `config.yaml` 里 `server.port` 改为其他值，需同步改这里的宿主端口与容器端口。

### 步骤 4：更新 README.md

- [ ] **步骤 4.1：替换 `README.md` 全文**

```markdown
# kdx-anthropic-bridge

> 让 Claude Code 透明对接多个上游 Anthropic 端点(科大讯飞科鼎、官方 Anthropic 等)的轻量网关。客户端按 proxy key 选择走哪个上游。

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## 解决什么问题

不同上游 Anthropic 端点对协议的适配程度不一,典型如科大讯飞科鼎(`maas-coding-api.cn-huabei-1.xf-yun.com/anthropic`,实际转接智谱 GLM):

1. **思维链(thinking)丢失** —— Claude Code 发 `thinking.type=adaptive`,科大适配层不认,响应不返回 thinking block
2. **WebSearch 失效** —— Claude Code 的 WebSearch 用服务端 `web_search_20250305`,科大不支持服务端搜索

本网关在中间做协议适配(按平台 profile 开关),修复上述问题,其他一切透传。每个上游绑定一个 `proxy_key`,客户端把它作为 `ANTHROPIC_AUTH_TOKEN` 即可显式选择上游。

```
Claude Code  ──HTTP──▶  kdx-anthropic-bridge  ──HTTPS──▶  科大 /anthropic   (profile=keding,改写)
                        (按 proxy key 路由)      ──HTTPS──▶  官方 api.anthropic.com (profile=official,透传)
```

## 快速开始

### 1. 配置

```bash
cp config.example.yaml config.yaml
```

编辑 `config.yaml`,至少配一个平台(填真实 `api_key` + 自定义 `proxy_key`):

```yaml
server:
  host: 0.0.0.0
  port: 8080

google_search:
  proxy: http://127.0.0.1:7890   # WebSearch 功能必填,谷歌直连会超时
  timeout: 15
  limit: 5

platforms:
  - name: kdx
    proxy_key: token-kdx-xxxxxx          # 自定义随机串,Claude Code 用它鉴权
    base_url: https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic
    api_key: appid:secret                # 上游真实 key
    profile: keding

profiles:
  keding:
    rewrite_thinking: true
    rewrite_web_search: true
    max_retries: 10
    retry_interval: 5s
    header_timeout: 30s
    parallel: 1
```

完整字段说明见 `config.example.yaml` 注释。

### 2. 启动

**Docker(推荐)**:

```bash
docker compose up -d
```

**本地运行**:

```bash
go build -o bin/bridge ./cmd/bridge
./bin/bridge                          # 默认读工作目录下 config.yaml
./bin/bridge --config /path/config.yaml   # 指定其他路径
```

### 3. 配置 Claude Code

把 Claude Code 的 `ANTHROPIC_BASE_URL` 指向网关,`ANTHROPIC_AUTH_TOKEN` 设为某平台的 `proxy_key`:

```env
ANTHROPIC_BASE_URL=http://localhost:8080
ANTHROPIC_AUTH_TOKEN=token-kdx-xxxxxx   # 与 config.yaml 里某平台的 proxy_key 一致
```

改 `~/.claude/settings.json` 的 `env` 段,或设环境变量。每个 Claude Code 实例配不同 `proxy_key` 即走不同上游。

### 4. 验证

```bash
# 思维链(kdx 平台)
claude -p "证明根号2是无理数,说一下推理过程"

# 网络搜索(kdx 平台)
claude -p "用 WebSearch 搜索 mitmproxy 最新版本号" --allowedTools WebSearch
```

思维链能看到推理过程,WebSearch 能返回真实链接,即正常。

## 处理的能力

| 能力 | 处理方式 |
|---|---|
| thinking 思维链 | 按 profile:开 `rewrite_thinking` 时 `adaptive` → `enabled` |
| WebSearch 网络搜索 | 按 profile:开 `rewrite_web_search` 时拦截 `web_search` tool_use 自行谷歌搜索 |
| 思考等级 effort | ✅ 透传(上游认 `output_config.effort`) |
| text / tool_use / 多轮工具循环 | ✅ 透传(上游原生支持) |
| 多上游路由 | ✅ 客户端用 proxy key 显式选择 |

## 路由原理

每个上游实例绑定唯一 `proxy_key`。客户端把它作为 `Authorization: Bearer <key>` 或 `x-api-key: <key>` 发起请求,网关提取 token 反查 `proxy_key → 平台`,命中则走该上游(注入上游真实 `api_key`),未命中返回 401。客户端的 proxy key 与上游真实 api_key 是两套,互不污染。

详见 [协议适配文档](docs/protocol-adaptation.md)。

## 文档

- [架构设计](docs/architecture.md)
- [协议适配](docs/protocol-adaptation.md)
- [开发指南](docs/development.md)

## 开发

```bash
go test ./... -v     # 测试
go vet ./...         # 静态检查
gofmt -l .           # 格式检查
```

## 上游重试与并发调优(profile 字段)

科大上游间歇性返回 502/503/429 或排队等首字节,profile 通过重试 + 并发抢窗口缓解:

- **`max_retries` / `retry_interval`**:上游返回 502/503/429 时按固定间隔重试。默认重试 10 次、间隔 5 秒(科大 profile 建议)。
- **`header_timeout`**:只限"等上游响应头"的时间(科大默认 30s),一旦开始流式返回就不再计时。
- **`parallel`**:单次请求并发发 N 路抢占游零星放行窗口,谁先拿到 200 就用谁,其余取消。默认 1(串行)。

> 提示:并发重试应保证总耗时在客户端超时内(`ANTHROPIC_API_TIMEOUT_MS`,Claude Code 默认 120s)。最坏耗时 ≈ max_retries × (响应头等待 + retry_interval)。

## 从旧版 `.env` 迁移

本版本起配置完全用 `config.yaml`,`.env` 已废弃。旧用户迁移映射:

| 旧 `.env` 字段 | 新 `config.yaml` 位置 |
|---|---|
| `KDX_PROXY_KEY` | `platforms[0].proxy_key` |
| `UPSTREAM_API_KEY` | `platforms[0].api_key` |
| `UPSTREAM_BASE_URL` | `platforms[0].base_url` |
| `UPSTREAM_MAX_RETRIES` | `profiles.<name>.max_retries` |
| `UPSTREAM_RETRY_INTERVAL_SEC` | `profiles.<name>.retry_interval`(`5s`) |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | `profiles.<name>.header_timeout`(`30s`) |
| `UPSTREAM_PARALLEL` | `profiles.<name>.parallel` |
| `PROXY_HOST` / `PROXY_PORT` | `server.host` / `server.port` |
| `GOOGLE_SEARCH_*` | `google_search.*` |

## License

[MIT](LICENSE)
```

- [ ] **步骤 4.2：Commit**

```bash
git add README.md docker-compose.yml go.mod go.sum .env .env.example
git commit -m "feat(config): 废弃 .env,配置全面迁移到 config.yaml,更新文档与 Docker"
```

> 说明:`.env` / `.env.example` 在步骤 1.1 已 `git rm` 暂存,此处 `git add` 把它们连同其他改动一起提交。

---

## 任务 6：全量验证

**文件：** 无（验证步骤）

- [ ] **步骤 1：gofmt 检查**

运行：

```bash
gofmt -l .
```

预期：无输出（所有文件格式规范）。若有输出，运行 `gofmt -w <列出的文件>` 后重检。

- [ ] **步骤 2：go vet**

运行：

```bash
go vet ./...
```

预期：无报错。

- [ ] **步骤 3：全量编译与测试**

运行：

```bash
go build ./...
go test ./... -v
```

预期：编译通过,全部测试 PASS。

- [ ] **步骤 4：端到端冒烟(可选,需真实上游 key)**

用真实 `config.yaml`(含 kdx + anthropic 两平台),启动:

```bash
go run ./cmd/bridge --config config.yaml
```

分别用两个 `proxy_key` 发请求,验证:
- kdx 平台:thinking 改写生效、WebSearch 返回真实链接。
- anthropic 平台:请求原样透传,正常返回。
- 未知 proxy key:返回 401。

启动日志应出现两行 `platform kdx -> ... (profile=keding)` 与 `platform anthropic -> ... (profile=official)`。

- [ ] **步骤 5：确认 CI 配置无需改**

`.github/workflows/ci.yml` 只跑 `gofmt`/`vet`/`build`/`test`,无 `.env` 依赖,本任务无需修改。人工确认该文件未引用已删除的 `.env`。

---

## 自检结果

### 1. 规格覆盖度

逐条对照规格章节:

- §1 目标(多上游、proxy key 路由、profile 改写)→ 任务 2、3 ✓
- §3.1 YAML 完整示例 → 任务 4 `config.example.yaml` ✓
- §3.2 字段语义 → 任务 2 结构体 + 任务 4 示例注释 ✓
- §3.3 校验规则(文件缺失/空 platforms/重复 key/引用缺失/port 非法)→ 任务 2 `validate` + `config_test` ✓
- §3.4 向后兼容(废弃 .env)→ 任务 5 ✓
- §4.2 数据结构 → 任务 2 ✓
- §4.3 鉴权层(pickPlatform/extractToken)→ 任务 2 ✓
- §4.4 每平台 Client → 任务 2 `platformRuntime` ✓
- §4.5 改写层 RewriteOptions → 任务 3 ✓
- §4.6 流式过滤触发条件(`HasWebSearch && RewriteWebSearch && searcher`)→ 任务 2 `handleAll` ✓
- §4.7 main.go 改造 → 任务 2 ✓
- §5 端到端数据流 → 任务 3 多平台测试 + 任务 6 冒烟 ✓
- §6 错误处理(401/log.Fatal/重试透传)→ 任务 2、3 ✓
- §7 测试策略(config_test/rewriter_test 扩展/handler_test 多平台/client 不变)→ 任务 2、3 ✓
- §8 待定项(--config/KnownFields/config.example.yaml)→ 待定项决议 + 任务 2、4 ✓
- §9 文档迁移(config.example.yaml/README/.gitignore/删 .env)→ 任务 4、5 ✓
- §10 实施顺序 → 任务 1-6 与之对应 ✓

无遗漏。

### 2. 占位符扫描

已扫描,无"待定/TODO/类似任务 N/添加适当错误处理"等占位符。所有代码步骤含完整代码块,所有命令含预期输出。

### 3. 类型一致性

- `RewriteOptions{Thinking, WebSearch bool}`:rewriter.go 定义(任务 3)↔ rewriter_test 调用(任务 3)↔ server.go `proxy.RewriteOptions{Thinking: p.profile.RewriteThinking, WebSearch: p.profile.RewriteWebSearch}`(任务 3)一致。
- `config.Duration`:config.go 定义(任务 2)↔ handler_test `config.Duration(30 * time.Second)`(任务 2、3)↔ server.go `time.Duration(prof.HeaderTimeout)` 转换(任务 2)一致。
- `platformRuntime{cfg, profile, client}`:server.go 定义(任务 2)↔ `s.byProxyKey[token]` 返回 `*platformRuntime`(任务 2)↔ handler_test `s.byProxyKey["..."].client.HTTP = ...`(任务 2、3)一致。
- `Profile` 字段名 `RewriteThinking`/`RewriteWebSearch`/`MaxRetries`/`RetryInterval`/`HeaderTimeout`/`Parallel`:config.go(任务 2)↔ server.go 引用(任务 2)↔ handler_test 构造(任务 2、3)一致。
- `Config.Index()` 返回 `map[string]*Platform`:config.go(任务 2)↔ config_test(任务 2)一致。
- `config.Load(path string)`:config.go(任务 2)↔ main.go `config.Load(*configPath)`(任务 2)一致。

类型一致,无拼写漂移。
