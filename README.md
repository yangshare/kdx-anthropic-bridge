# kdx-anthropic-bridge

> 让 Claude Code 在科大讯飞 Anthropic 端点上正常工作的轻量透明代理。

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## 解决什么问题

Claude Code 直连科大讯飞 Anthropic 端点(`maas-coding-api.cn-huabei-1.xf-yun.com/anthropic`,实际转接智谱 GLM)时,两个能力丢失:

1. **思维链(thinking)丢失** —— Claude Code 发 `thinking.type=adaptive`,科大适配层不认此格式,响应不返回 thinking block,代码质量下降
2. **WebSearch 失效** —— Claude Code 的 WebSearch 用 Anthropic 服务端 `web_search_20250305`,科大不支持服务端搜索,返回空壳

本代理在中间做协议适配,修复这两个问题,其他一切透传。

```
Claude Code  ──HTTP──▶  kdx-anthropic-bridge  ──HTTPS──▶  科大 /anthropic
                        (thinking 改写 + web_search 拦截)
```

## 快速开始

### 1. 配置

```bash
cp .env.example .env
```

编辑 `.env`:

```env
# 代理自身 key(Claude Code 用它当 ANTHROPIC_AUTH_TOKEN,自己设个随机串)
KDX_PROXY_KEY=your-random-proxy-key

# 科大上游 key(appid:secret 格式)
UPSTREAM_API_KEY=your-keding-key

# 科大上游端点(默认值已填好)
UPSTREAM_BASE_URL=https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic

# 监听
PROXY_HOST=0.0.0.0
PROXY_PORT=8080

# 谷歌搜索代理(WebSearch 功能必填,谷歌直连会超时)
GOOGLE_SEARCH_PROXY=http://127.0.0.1:7890
GOOGLE_SEARCH_TIMEOUT=15
GOOGLE_SEARCH_LIMIT=5
```

### 2. 启动

**Docker(推荐)**:

```bash
docker compose up -d
```

**本地运行**:

```bash
go build -o bin/bridge ./cmd/bridge
./bin/bridge    # 从项目根目录运行,会自动读 .env
```

### 3. 配置 Claude Code

把 Claude Code 的 `ANTHROPIC_BASE_URL` 和 `ANTHROPIC_AUTH_TOKEN` 改为指向代理:

```env
ANTHROPIC_BASE_URL=http://localhost:8080
ANTHROPIC_AUTH_TOKEN=your-random-proxy-key   # 与 .env 里 KDX_PROXY_KEY 一致
```

改 `~/.claude/settings.json` 的 `env` 段,或设环境变量。

### 4. 验证

```bash
# 思维链
claude -p "证明根号2是无理数,说一下推理过程"

# 网络搜索
claude -p "用 WebSearch 搜索 mitmproxy 最新版本号" --allowedTools WebSearch
```

思维链能看到推理过程,WebSearch 能返回真实链接,即正常。

## 处理的能力

| 能力 | 处理方式 |
|---|---|
| thinking 思维链 | ✅ 改写 `adaptive` → `enabled` |
| WebSearch 网络搜索 | ✅ 代理内置谷歌搜索,拦截 `web_search` tool_use 自行搜索,改写成 `web_search_tool_result` 返回 |
| 思考等级 effort | ✅ 透传(上游认 `output_config.effort`) |
| text / tool_use / 多轮工具循环 | ✅ 透传(上游原生支持) |
| stop_reason / usage | ✅ 透传 |

## 工作原理

- **thinking**:请求侧把 `thinking.type` 从 `adaptive` 改写成 `enabled`,上游即返回 thinking block
- **WebSearch**:
  - 请求侧把 `web_search_20250305` 服务端工具改写成普通 function tool(带 query input_schema)
  - 响应侧流式拦截 `web_search` tool_use,调内置谷歌搜索,改写成 `server_tool_use` + `web_search_tool_result` 返回

详见 [协议适配文档](docs/protocol-adaptation.md)。

## 文档

- [架构设计](docs/architecture.md)
- [协议适配](docs/protocol-adaptation.md) —— 每个问题的根因和修法
- [开发指南](docs/development.md) —— 本地运行、测试、抓包调试

## 开发

```bash
go test ./... -v     # 测试
go vet ./...         # 静态检查
gofmt -l .           # 格式检查
```

见 [开发指南](docs/development.md)。

## 已知限制

- **WebSearch 依赖代理**:`GOOGLE_SEARCH_PROXY` 配的代理不通时,WebSearch 失效(thinking 不受影响)。
- **谷歌 DOM 变化**:解析依赖 Google 页面结构,改版可能失效。

## License

[MIT](LICENSE)
