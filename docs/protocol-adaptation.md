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

## 5. 上游重试(502/503/429)

### 现象

上游(科大→智谱 GLM)偶发返回 503 "The system is busy, please try again later"。
Claude Code 客户端对这类错误用硬编码指数退避重试,间隔长、次数少,单次请求平白浪费约一分钟。
且客户端的重试节奏不可配置(实测 native 二进制只有 `API_TIMEOUT_MS` / `MAX_RETRIES` 两个旋钮,
无退避间隔配置项)。

### 修法

把重试下沉到 bridge:bridge 收到 Claude Code 请求后,若上游返回 502/503/429,
按**固定间隔**重试,直到成功或达上限,再把最终响应透传给 Claude Code。
这样 Claude Code 感知到的是"稍等即成功"或"重试耗尽后的错误",不再干等客户端长退避。

实现见 `internal/upstream/client.go` 的 `Forward`:

- 重试状态码:502、503、429(网关类瞬时错误)
- 非重试状态码(401/400/500 等):立即返回,不重试
- 网络错误(上游不可达):同样重试,视为瞬时故障
- 请求体以 `[]byte` 传入,每次重试用 `bytes.NewReader` 重新构造,保证可重放
- 重试耗尽:返回最后一次响应(让下游看到真实错误状态码),不隐瞒

### 配置

重试参数在 `config.yaml` 的 `profiles.<name>` 下按平台配置(各平台互不影响):

| 字段 | 不填默认 | 说明 |
|---|---|---|
| `max_retries` | 0(不重试) | 502/503/429 最大重试次数(不含首次) |
| `retry_interval` | 0(不等待) | 重试间隔,带单位字符串(如 `5s`);>0 才 sleep |

profile 下还有 `header_timeout`(等响应头超时,默认 `30s`)、`parallel`(并发抢窗口,默认 1)等字段,
完整示例见 `config.example.yaml`。

### 注意

bridge 重试期间 Claude Code 客户端在等同一个 HTTP 响应,需把客户端 `API_TIMEOUT_MS`
调大到能覆盖最坏情况(`max_retries × retry_interval`,如 10 × 5s = 50s)。
否则客户端会先超时断开,bridge 仍在重试,浪费上游配额。

## 已知限制

- **谷歌搜索依赖代理**:`GOOGLE_SEARCH_PROXY` 配的代理不通时,WebSearch 失效(thinking 不受影响)。
- **谷歌 DOM 变化**:解析依赖 Google 页面结构,改版可能失效。
