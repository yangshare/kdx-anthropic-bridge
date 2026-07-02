# 架构

## 定位

kdx-anthropic-bridge 是一个 Anthropic 协议透明代理,部署在 Claude Code 和上游 Anthropic 兼容端点之间。

它只做**协议适配**——修复上游端点对某些 Anthropic 字段处理不全导致 Claude Code 能力丢失的问题。其他一切透传。

```
Claude Code  ──HTTP──▶  kdx-anthropic-bridge  ──HTTPS──▶  上游 Anthropic 端点
                        (协议适配)
```

## 解决的问题

上游端点(科大讯飞,转接智谱 GLM)对两个 Anthropic 能力适配不全:

1. **thinking 思维链丢失**:Claude Code 发 `thinking.type=adaptive`,上游不认此格式,响应不返回 thinking block
2. **WebSearch 失效**:Claude Code 的 WebSearch 用 Anthropic 服务端 `web_search_20250305`,上游不支持服务端搜索

## 分层

| 层 | 包 | 职责 |
|---|---|---|
| 接入层 | `internal/server` | HTTP 服务、鉴权、路由、流式回传 |
| 业务层 | `internal/proxy` | 请求改写(thinking、web_search 工具)、响应流式拦截转换 |
| 基础层 | `internal/upstream` | 上游 HTTP 客户端、流式转发 |
| 能力层 | `internal/search` | 谷歌搜索(chromedp 渲染 + goquery 解析) |
| 配置层 | `internal/config` | .env 加载 |
| 协议层 | `internal/anthropic` | Anthropic 协议常量 |

层间规则:每层只向下依赖,接入层不写业务判断,业务层不碰 HTTP 细节。

## 数据流

### 普通请求(text / tool_use / 多轮)

```
Claude Code 请求 → 鉴权 → 改写 thinking(如需)→ 转发上游 → 响应流式透传
```

### WebSearch 请求

```
Claude Code 请求(带 web_search_20250305 工具)
  ↓
请求侧改写:web_search_20250305 → 普通 function tool(带 query input_schema)
  ↓
转发上游 → 上游让模型发起 tool_use(name=web_search, input 带 query)
  ↓
响应侧流式拦截:
  - 检测到 web_search tool_use block
  - 从 input_json_delta 拼 query
  - 调谷歌搜索(chromedp)
  - 改写成 server_tool_use + web_search_tool_result 返回给 Claude Code
```

## 关键设计

### 透传优先

- 用 `map[string]any` 解析请求体,只动需要改的字段,其他原样保留
- 没有需要改的字段时,返回原始 body(避免重新序列化改变字节顺序)
- 响应除 web_search block 外逐字节透传,保持流式体验

### 流式处理

- 上游响应是 SSE 流,代理**不缓冲整个响应**
- `StreamFilter` 行级解析 SSE,检测到 web_search tool_use 时暂停该 block 转发,搜索后改写输出
- 其他 block 立即透传,不引入额外延迟

### WebSearch 搜索实现

内置谷歌搜索,复刻 [ai_stock](https://github.com/) 项目的 PlaywrightSearchService 逻辑(Go 重写):

- `chromedp` 模拟浏览器直连 Google(反爬关键:`--disable-blink-features=AutomationControlled`)
- `goquery` 解析结果 HTML(h3 锚点 → a[href] → 摘要)
- 经配置的代理访问谷歌(谷歌直连会超时)

## 配置

见 [README](../README.md) 的快速开始。关键配置:

| 配置 | 说明 |
|---|---|
| `KDX_PROXY_KEY` | 代理自身鉴权 key,Claude Code 用它当 `ANTHROPIC_AUTH_TOKEN` |
| `UPSTREAM_API_KEY` | 上游端点 key |
| `UPSTREAM_BASE_URL` | 上游 Anthropic 端点基址 |
| `GOOGLE_SEARCH_PROXY` | 谷歌搜索代理(必填,谷歌直连超时) |
| `PROXY_PORT` | 代理监听端口 |

## 部署

支持 Docker 部署。容器需 host 网络模式(访问宿主机代理)或配置好代理可达性。

见 [README](../README.md) 的快速开始。
