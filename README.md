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
