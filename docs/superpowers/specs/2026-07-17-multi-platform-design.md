# 多平台上游支持 — 设计规格

- 日期：2026-07-17
- 状态：待审查
- 主题：将单上游（科大专用桥）改造为多上游供应商网关，按 proxy key 路由，配置全面迁移到 YAML

## 1. 背景与目标

### 1.1 现状

项目当前是**单上游专用桥**：只对接科大讯飞科鼎的 Anthropic 端点。证据：

- `internal/config/config.go` — `Config` 只有一组上游字段（`UpstreamBaseURL` / `UpstreamAPIKey`），无切片、无映射。
- `internal/server/server.go:31` `New()` — 用 `cfg.UpstreamBaseURL` / `cfg.UpstreamAPIKey` 构造**一个** `upstream.Client`，存于 `s.upstream`。
- `internal/proxy/rewriter.go` — 包注释即写明"改写为**科大端点认识的** enabled"、`web_search` 改写因"**科大不支持**服务端 web_search"。改写逻辑是为科大量身定做。
- `.env` — 单组 `UPSTREAM_BASE_URL` / `UPSTREAM_API_KEY` / `UPSTREAM_*` 调优参数。

### 1.2 目标

支持**多个上游供应商**（如科大、官方 Anthropic、其他 MaaS），下游仍以 Claude Code 为主。每个请求由客户端通过**鉴权 key 显式指定**走哪个上游，网关不做策略猜测。不同平台的协议改写差异由**平台 profile** 封装。

### 1.3 非目标（YAGNI）

- 不做多下游客户端协议（Cline / Continue 等）—— 下游仍只认 Anthropic 协议。
- 不做负载均衡 / 轮询 / 主备故障转移 —— 客户端显式选上游，网关只做"按 key 分发"。
- 不做按 `model` 字段路由 —— 路由键只有 proxy key。
- 不做配置热重载 —— 改 YAML 需重启进程。

## 2. 关键决策

| 维度 | 决定 |
|---|---|
| 多平台含义 | 多个上游供应商 |
| 路由方式 | 客户端显式指定 |
| 标识传递 | proxy key 区分上游 |
| 改写差异 | YAML 完整定义 profile |
| 配置载体 | YAML 管全部，`.env` 废弃 |

**路由原理**：每个上游实例绑定一个唯一的 `proxy_key`。客户端把该 key 作为 `ANTHROPIC_AUTH_TOKEN` 发起请求，网关从 `Authorization: Bearer <key>` 或 `x-api-key: <key>` 提取 token，反查 `map[proxy_key]→Platform`，命中则走该上游。`upstream.Client.doOnce()` 已有"替换鉴权头为上游 key"的逻辑，天然支持——客户端的 proxy key 与上游真正的 api_key 是两套，互不污染。

## 3. 配置模型（YAML）

引入 `config.yaml`，**完全取代** `.env`。模块新增依赖 `gopkg.in/yaml.v3`。

### 3.1 完整示例

```yaml
# 网关自身
server:
  host: 0.0.0.0
  port: 8080

# 谷歌搜索（WebSearch 功能；proxy 留空则禁用响应侧 web_search 拦截）
google_search:
  proxy: http://127.0.0.1:7890
  timeout: 15        # 秒
  limit: 5

# 上游实例列表：每个实例绑定一个 proxy_key（客户端用它触发该上游）
platforms:
  - name: kdx
    proxy_key: token-kdx-xxxxxx
    base_url: https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic
    api_key: appid:secret
    profile: keding          # 引用 profiles.keding

  - name: anthropic
    proxy_key: token-anthropic-yyyyyy
    base_url: https://api.anthropic.com
    api_key: sk-ant-...
    profile: official

# 改写模板：封装该平台的全部适配规则 + 重试/并发参数
profiles:
  keding:
    rewrite_thinking: true       # thinking.type=adaptive -> enabled
    rewrite_web_search: true     # web_search_20250305 -> function tool
    max_retries: 10              # 502/503/429 重试次数（不含首次）
    retry_interval: 5s
    header_timeout: 30s          # 等上游响应头的超时
    parallel: 1                  # 单次 attempt 并发抢窗口路数

  official:
    rewrite_thinking: false      # 官方原生支持，不改写
    rewrite_web_search: false
    max_retries: 0
    header_timeout: 60s
    parallel: 1
```

### 3.2 字段语义

**`platforms[]`（上游实例）**

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` | 是 | 上游标识，仅用于日志/错误信息，不参与路由 |
| `proxy_key` | 是 | 客户端侧鉴权 token，即路由键。全局唯一，重复则启动报错 |
| `base_url` | 是 | 上游基址（如 `https://api.anthropic.com`） |
| `api_key` | 是 | 上游真正的 key；转发时注入到 `Authorization` / `x-api-key` |
| `profile` | 是 | 引用 `profiles` 下的模板名，不存在则启动报错 |

**`profiles.<name>`（改写模板）**

| 字段 | 默认 | 说明 |
|---|---|---|
| `rewrite_thinking` | `false` | `true` 时把 `thinking.type=adaptive` 改写为 `enabled`（科大需要） |
| `rewrite_web_search` | `false` | `true` 时把 `web_search_20250305` 服务端工具改写为带 `query` 的 function tool（科大需要） |
| `max_retries` | `0` | 502/503/429 重试次数（不含首次）。0 = 不重试 |
| `retry_interval` | `0s` | 重试间隔。仅 `>0` 才 sleep |
| `header_timeout` | `30s` | 等上游响应头超时；流式开始后不再计时 |
| `parallel` | `1` | 单次 attempt 并发抢窗口路数。`<=1` 串行 |

**`server` / `google_search`**：与现有 `.env` 字段一一对应，名称转 snake_case（`PROXY_HOST`→`server.host`、`GOOGLE_SEARCH_PROXY`→`google_search.proxy` 等）。

### 3.3 校验规则（启动时）

1. `config.yaml` 不存在 → 致命错误，提示路径。
2. `platforms` 为空 → 致命错误（至少要有一个上游）。
3. 任意 `proxy_key` 重复 → 致命错误（路由二义性）。
4. 任意 `profile` 引用不存在的模板名 → 致命错误。
5. `profiles` 为空但 `platforms` 引用了 profile → 致命错误。
6. `server.port` 非法 → 致命错误。

校验全在 `config.Load()` 完成，失败即 `log.Fatal`，运行期不再处理配置错误。

### 3.4 向后兼容

`.env` **整体废弃**，不保留双轨。迁移由 `config.example.yaml` + 文档承担（见第 9 节）。这是显式破坏性变更，需在 commit / README 标注。

## 4. 架构与组件

### 4.1 改造范围总览

改造集中在**配置层 + 鉴权层 + server 选平台**；`upstream.Client`（重试 + 并发抢窗口）、`proxy` 改写与流式过滤逻辑**基本原样复用**。

```
请求 ─► server.handleAll
          │
          ├─ pickPlatform(r)         [新增] 从 Authorization/x-api-key 提取 token
          │                            反查 map[proxy_key]*Platform，未命中 → 401
          │
          ├─ rewriter(profile, body) [改造] rewriter 按 profile 开关改写
          │
          ├─ platform.Client.Forward [改造] 每个 platform 持有自己的 *upstream.Client
          │                            (注入 profile 的重试/并发/超时 + 平台 base_url/api_key)
          │
          └─ 流式回传 (改写 web_search 拦截按 profile.HasWebSearch)
```

### 4.2 数据结构（`internal/config/config.go` 重写）

```go
type Config struct {
    Server      ServerConfig
    GoogleSearch GoogleSearchConfig
    Platforms   []Platform            // 有序，保留配置顺序
    Profiles    map[string]Profile    // name -> 模板
}

type ServerConfig struct {
    Host string
    Port int
}

type GoogleSearchConfig struct {
    Proxy   string
    Timeout int
    Limit   int
}

type Platform struct {
    Name     string
    ProxyKey string
    BaseURL  string
    APIKey   string
    Profile  string   // 引用 Profiles 的 key
}

type Profile struct {
    RewriteThinking  bool
    RewriteWebSearch bool
    MaxRetries       int
    RetryInterval    time.Duration
    HeaderTimeout    time.Duration
    Parallel         int
}
```

`Config.Load(path string)`：读 YAML → `yaml.Unmarshal` → 解析 `retry_interval`/`header_timeout`（`time.Duration` 需自定义 unmarshal 或用字符串 + `time.ParseDuration`）→ 执行第 3.3 节校验。

新增 `Config.Index() map[string]*Platform`：构造 `proxy_key → Platform` 反查表（含预解析的 profile 副本），供鉴权层 O(1) 查找。为转发构造方便，`Platform` 运行态可挂一个 `*upstream.Client`（见 4.4）。

### 4.3 鉴权层改造（`internal/server/server.go`）

现状 `authorized(r)` 把 token 与单一 `cfg.ProxyKey` 比对。改造为：

```go
// pickPlatform 从请求头提取 token，反查命中则返回该平台；未命中返回 nil。
func (s *Server) pickPlatform(r *http.Request) *Platform {
    token := extractToken(r)         // Authorization: Bearer <t> 或 x-api-key: <t>
    if token == "" {
        return nil
    }
    return s.byProxyKey[token]       // map[string]*Platform
}
```

`handleAll` 开头：

```go
platform := s.pickPlatform(r)
if platform == nil {
    writeError(w, http.StatusUnauthorized, "invalid or unknown proxy key")
    return
}
// 后续 rewriter / Forward 都用这个 platform
```

`extractToken` 复用现有 `authorized` 的解析逻辑（Bearer 前缀剥离 + x-api-key），仅把"比对"换成"查表"。

### 4.4 上游客户端的归属

现状 `Server` 持有单个 `*upstream.Client`。改造为**每个 `Platform` 持有自己的 `*upstream.Client`**：

- `upstream.Client` **不变**（保留 `BaseURL` / `APIKey` / `MaxRetries` / `RetryInterval` / `Parallel` / `HTTP`）。
- `server.New()` 遍历 `cfg.Platforms`，为每个平台构造一个 `*upstream.Client`，注入该平台 profile 的 `HeaderTimeout`（作 transport 的 `ResponseHeaderTimeout`）、`MaxRetries`、`RetryInterval`、`Parallel`，以及平台的 `BaseURL` / `APIKey`。
- 挂在运行态结构上：给 `Platform` 增加运行态字段 `Client *upstream.Client`（配置加载后为 nil，`server.New()` 填充）。鉴权层 `pickPlatform` 返回的 `*Platform` 即可直接取 `platform.Client.Forward(...)`，无需 server 再维护第二张映射表。

每个平台独立 `http.Transport`（因 `ResponseHeaderTimeout` 随 profile 变化），平台间互不干扰。

### 4.5 改写层改造（`internal/proxy/rewriter.go`）

现状 `RewriteRequest(body)` 无条件做科大改写。改造为**按 profile 开关**：

```go
type RewriteOptions struct {
    Thinking   bool   // profile.RewriteThinking
    WebSearch  bool   // profile.RewriteWebSearch
}

func RewriteRequest(body []byte, opt RewriteOptions) (*RewriteResult, error)
```

- `rewriteThinking` / `rewriteWebSearchTools` 内部各加 `if !opt.XXX { return ... }` 短路。
- 两者全关时（如 `official` profile）返回原始 body 不序列化，等同透传。
- `HasWebSearch` 语义不变：仍返回"请求是否含 web_search 工具"，但**只有 profile 开了 `rewrite_web_search` 时**才会触发响应侧拦截（见 4.6）。未开 profile 的平台若请求带 web_search，原样透传（上游自己处理）。

### 4.6 流式过滤层（`internal/proxy/stream_filter.go`）— 基本不动

`StreamFilter` 的 web_search 拦截逻辑不变。触发条件由 `handleAll` 控制：

- 仅当 `profile.RewriteWebSearch == true` 且 `result.HasWebSearch == true` 且配置了 `google_search.proxy` 时，才走 `FilterStream`。
- 否则原样流式透传。

`google_search` 仍是**全局唯一一份**（不分平台）——所有平台的 web_search 都走同一个谷歌搜索代理。

### 4.7 `main.go` 改造

- 删除 `godotenv.Load()`，改为读取 `config.yaml` 路径（默认工作目录，可用 `--config` flag 或 `CONFIG_PATH` 覆盖——见第 8 节待定）。
- `log.Printf` 的 upstream 地址改为遍历打印所有平台：`platform kdx -> https://... (profile=keding)`。

## 5. 数据流（端到端）

以 Claude Code 发 `/v1/messages` 走科大为例：

1. Claude Code 配 `ANTHROPIC_AUTH_TOKEN=token-kdx-xxxxxx`，POST `/v1/messages`，带 `Authorization: Bearer token-kdx-xxxxxx`。
2. `handleAll` → `pickPlatform` 提取 `token-kdx-xxxxxx` → 命中 `Platform{kdx}` → 取其 profile `keding` 与 `*upstream.Client`。
3. 路径为 `/v1/messages` + POST → `RewriteRequest(body, RewriteOptions{Thinking:true, WebSearch:true})` → 改写 adaptive→enabled、web_search→function tool，返回 `HasWebSearch=true`。
4. `platform.Client.Forward(method, path, body, headers)` → 注入科大 `api_key`（替换掉客户端的 proxy key）→ 重试 + 并发抢窗口 → 拿到响应。
5. `HasWebSearch && profile.RewriteWebSearch && searcher!=nil` → `FilterStream` 拦截 web_search tool_use 并自行谷歌搜索注入结果。
6. 流式回传给 Claude Code。

走官方 Anthropic 时：`pickPlatform` 命中 `anthropic` 平台 → profile `official`（两开关全 false）→ `RewriteRequest` 原样透传 → `Forward` 用官方 key 转发 → 不走 web_search 拦截，原样透传。

## 6. 错误处理

| 场景 | 行为 |
|---|---|
| token 未提供或不匹配任何平台 | 401 `invalid or unknown proxy key` |
| `config.yaml` 缺失/格式错 | 启动 `log.Fatal`，提示路径与解析错误 |
| `platforms`/`profiles` 校验失败（重复 key、引用缺失） | 启动 `log.Fatal`，列出具体冲突项 |
| 上游返回 502/503/429 | 按该平台 profile 的 `max_retries`/`retry_interval` 重试，耗尽后透传最后响应（行为同现状） |
| 上游网络错误 | 同上，纳入重试 |
| web_search 拦截期谷歌搜索失败 | 沿用现有 `StreamFilter` 的失败处理（记日志，结果降级） |
| 未知 profile 开关字段（YAML 有拼写错误） | yaml.v3 默认忽略未知字段；若需严格，设 `KnownFields(true)` 启动报错（见第 8 节待定） |

错误响应格式沿用现有 `writeError`（`{"error":{"type":"proxy_error","message":"..."}}`）。

## 7. 测试策略

延续现有表驱动风格（参考 `rewriter_test.go` / `client_test.go` / `handler_test.go`）。

1. **`config_test.go`（新增）**：
   - 解析完整示例 YAML → 字段正确。
   - `retry_interval`/`header_timeout` 字符串解析（`5s`→`5*time.Second`）。
   - 校验：空 platforms、重复 proxy_key、引用不存在 profile → 各自返回预期错误。
   - `Index()` 反查表正确性。

2. **`rewriter_test.go`（扩展）**：
   - `RewriteOptions{Thinking:true,WebSearch:true}` → 现有用例不变。
   - `RewriteOptions{Thinking:false,WebSearch:false}` → body 原样返回（不序列化）。
   - 仅开其一 → 只改对应字段。

3. **`handler_test.go`（扩展）**：
   - 多平台路由：用 `token-kdx` 命中 kdx、用 `token-anthropic` 命中 anthropic、用未知 token 返回 401。
   - kdx 平台请求被改写、anthropic 平台请求透传（用 mock upstream 验证收到的 body）。
   - `newTestServer` 构造改为注入多平台配置。

4. **`client_test.go`**：现状重试/并发用例不变（`Client` 未改）。

5. **端到端冒烟**：本地起两个 mock 上游，两个 proxy key 各打到对应上游，验证路由与改写。

## 8. 待定项（需用户拍板，实现计划前确认）

1. **配置文件路径指定方式**：`--config` flag vs `CONFIG_PATH` 环境变量 vs 固定工作目录 `config.yaml`。推荐 `--config` flag + 默认 `./config.yaml`。
2. **YAML 未知字段策略**：是否启用 `yaml.Decoder.KnownFields(true)` 严格模式（拼错字段名启动即报错）。推荐启用。
3. **示例配置文件名**：`config.example.yaml`（随仓库提交，真实 `config.yaml` 进 `.gitignore`）。

## 9. 文档与迁移

- 新增 `config.example.yaml`，含 kdx + anthropic 双平台示例。
- 更新 `README`：`.env` 章节替换为 `config.yaml` 章节；说明每个 Claude Code 实例配对应 `proxy_key`。
- `.gitignore` 增加 `config.yaml`（含真实 key），保留 `config.example.yaml`。
- 删除 `.env` / `.env.example`（或保留并标注 DEPRECATED 指向新文档）。推荐直接删除，避免混淆。
- 迁移说明：现有 `.env` 用户需把 `UPSTREAM_BASE_URL/API_KEY` + `KDX_PROXY_KEY` 映射为单个 kdx 平台条目。

## 10. 实施顺序（供 writing-plans 参考）

1. 加 `gopkg.in/yaml.v3` 依赖。
2. 重写 `internal/config`（Config 结构 + YAML 加载 + 校验 + Index）+ 测试。
3. 改造 `internal/proxy/rewriter.go`（`RewriteOptions`）+ 测试。
4. 改造 `internal/server`（pickPlatform + 每平台 Client + handleAll 改写调用）+ 测试。
5. 改造 `cmd/bridge/main.go`（读 config.yaml）。
6. 新增 `config.example.yaml`，更新 `.gitignore` / README，删除 `.env*`。
7. CI（`.github/workflows/ci.yml`）若有 `.env` 依赖需同步改。
