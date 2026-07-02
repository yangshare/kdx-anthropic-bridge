# 协议适配

本文记录 kdx-anthropic-bridge 做的协议适配:每个问题的现象、根因、修法。所有结论基于真实抓包验证。

## 1. thinking 思维链

### 现象

Claude Code 直连上游端点时,响应里没有 thinking block,模型不展示推理过程,代码质量下降。

### 根因

Claude Code 发送的请求体里,thinking 字段是:

```json
"thinking": {"type": "adaptive", "display": "omitted"}
```

`adaptive` 是 Anthropic 较新的 thinking 类型。上游端点的 Anthropic 适配层**不认 `adaptive` 类型**,收到后不开启思维链,响应里不返回 thinking block。

### 修法

请求侧改写:`adaptive` → `enabled`。

```json
// 改写前
"thinking": {"type": "adaptive", "display": "omitted"}

// 改写后
"thinking": {"type": "enabled"}
```

实现见 `internal/proxy/rewriter.go` 的 `rewriteThinking`。去掉 `display` / `budget_tokens` 等子字段(上游不认 budget_tokens,且不影响思考深度)。

### 验证

改写后上游响应含完整 `thinking_delta` 事件流,思维链正常返回。

## 2. 思考等级(effort)

### 现象

Claude Code 用 `/effort` 设思考等级,发 `output_config.effort`(high/medium/low/max)。

### 根因与修法

`output_config.effort` 在上游端点**有效**(实测 effort=low 与 max 的 thinking 深度有差异),Claude Code 已发此字段,**代理透传即可,无需处理**。

注:上游不认智谱原生的 `reasoning_effort` 字段(Anthropic 端点只认 Anthropic 协议的 `thinking` + `output_config.effort`)。

## 3. WebSearch 网络搜索

### 现象

Claude Code 调 WebSearch 工具,返回空壳占位文案("I'll search for..."),拿不到真实搜索结果。对比 deepseek / 智谱官方 Anthropic 端点,WebSearch 正常返回真实结果。

### 根因

Claude Code 的 WebSearch 工具用的是 **Anthropic 服务端 `web_search_20250305` 工具**(源码 `WebSearchTool.ts`):

```ts
function makeToolSchema(input: Input): BetaWebSearchTool20250305 {
  return {
    type: 'web_search_20250305',
    name: 'web_search',
    max_uses: 8,
  }
}
```

搜索由**上游供应商在服务端执行**,响应返回 `server_tool_use` + `web_search_tool_result`(带 title/url)。

**上游端点不支持服务端 web_search**:
- 把 `web_search_20250305` 退化成普通 function tool,模型发起 `input` 为空的 `tool_use`(死循环)
- 没有 `server_tool_use` block,没有 `web_search_tool_result` block
- Claude Code 拿不到搜索结果,回填空壳

对比 deepseek 官方端点:同样请求返回 `server_tool_use` + `web_search_tool_result`(带真实 title/url),搜索在服务端执行。

### 修法

代理内置谷歌搜索,绕过上游不支持服务端搜索的问题。两步:

**请求侧改写**(`internal/proxy/rewriter.go` 的 `rewriteWebSearchTools`):

把 `web_search_20250305` 服务端工具定义,改写成普通 function tool(带 query 的 input_schema):

```json
// 改写前(Claude Code 发的)
{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}

// 改写后(代理改成)
{
  "name": "web_search",
  "description": "Search the web. Returns a list of results with title and url.",
  "input_schema": {
    "type": "object",
    "properties": {"query": {"type": "string", "description": "The search query"}},
    "required": ["query"]
  }
}
```

这样模型发起的 tool_use 带 query(而非空 input),代理能拿到 query 去搜索。

**响应侧流式拦截**(`internal/proxy/stream_filter.go`):

- 流式解析上游 SSE
- 检测 `tool_use name=web_search` 的 block,从 `input_json_delta` 拼 query
- 调谷歌搜索(`internal/search`)
- 改写成 `server_tool_use`(带 input_json_delta 流式输出 query)+ `web_search_tool_result`(带真实 title/url)返回给 Claude Code

输出格式参照 deepseek 真实响应,Claude Code 源码(`WebSearchTool.ts` 的 `makeOutputFromSearchResponse`)只取 `title` + `url`。

### 谷歌搜索实现

复刻 ai_stock 项目的 PlaywrightSearchService 逻辑(Go 重写),见 `internal/search`:

- `google.go`:chromedp 模拟浏览器直连 Google
- `parser.go`:goquery 解析结果 HTML(h3 锚点 → a[href] → 摘要)

**反爬关键**:chromedp 启动时加 `--disable-blink-features=AutomationControlled`,否则 Google 返回 captcha 页。已验证有效。

## 4. 其他能力(透传)

以下能力上游原生支持,代理直接透传:

| 能力 | 说明 |
|---|---|
| text block | `text_delta` 流式文本 |
| tool_use(客户端 function tool) | `input_json_delta` 流式工具调用 |
| 多工具并行 | 单响应内多个 tool_use block |
| tool 循环 | user→assistant(tool_use)→user(tool_result)→assistant 多轮 |
| stop_reason | `tool_use` / `end_turn` 正确返回 |
| system prompt | text block + ephemeral cache_control |
| usage | 上游返回(可能为 0,不影响功能) |

## 已知限制

- **谷歌搜索依赖代理**:`GOOGLE_SEARCH_PROXY` 配的代理不通时,WebSearch 失效(thinking 不受影响)。
- **谷歌 DOM 变化**:解析依赖 Google 页面结构,改版可能失效。
