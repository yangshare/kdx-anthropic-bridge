# 开发指南

## 环境要求

- Go 1.26+
- (可选)Docker,用于容器化部署
- (可选)mitmproxy,用于抓包调试

## 本地运行

```bash
# 编译
go build -o bin/bridge ./cmd/bridge

# 配置(复制 .env.example 改)
cp .env.example .env
# 编辑 .env,填 KDX_PROXY_KEY / UPSTREAM_API_KEY / GOOGLE_SEARCH_PROXY

# 运行(从项目根目录,会自动读 .env)
./bin/bridge
```

bridge 监听 `PROXY_HOST:PROXY_PORT`,启动日志:
```
kdx-anthropic-bridge listening on 0.0.0.0:8080
upstream: https://...
```

## 测试

```bash
go test ./... -v     # 全部测试
go vet ./...         # 静态检查
gofmt -l .           # 格式检查(无输出=OK)
```

测试覆盖:
- `internal/proxy/rewriter_test.go`:请求改写(thinking + web_search 工具)
- `internal/proxy/stream_filter_test.go`:响应流式拦截
- `internal/search/parser_test.go`:谷歌 HTML 解析
- `internal/server/handler_test.go`:集成测试(httptest 假上游)

## 端到端验证

起 bridge 后,用 Claude Code 经它调上游:

```bash
# 配 Claude Code 指向 bridge
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN=<你 .env 里的 KDX_PROXY_KEY>

# 测 thinking
claude -p "证明根号2是无理数"

# 测 WebSearch
claude -p "用 WebSearch 搜索 mitmproxy 最新版本号" --allowedTools WebSearch
```

## 抓包调试

`scripts/` 下有 mitmproxy 抓包脚本,用于调试协议问题:

- `capture_kdx.py`:抓 Claude Code → 上游的流量(只抓科大域名)
- `capture_all.py`:抓所有域名流量,按 DS/KD/OTH 分类
- `capture_bridge.py`:抓 Claude Code → bridge 的流量(看 bridge 输出)

用法:
```bash
mitmdump -s scripts/capture_bridge.py -p 8889
# 然后让 Claude Code 经 mitmproxy(设 HTTPS_PROXY=http://127.0.0.1:8889)
```

抓包结果落在 `captures/`(已在 .gitignore,不入库)。

## 代码结构

```
cmd/bridge/main.go              # 入口
internal/
├── config/                     # .env 加载
├── anthropic/                  # Anthropic 协议常量
├── proxy/
│   ├── rewriter.go             # 请求改写(thinking + web_search 工具)
│   ├── stream_filter.go        # 响应流式拦截(web_search tool_use → 搜索 → 改写)
│   └── adapter.go              # 搜索执行器适配
├── search/
│   ├── google.go               # chromedp 谷歌搜索
│   └── parser.go               # goquery HTML 解析
├── server/                     # HTTP 服务、鉴权、路由
└── upstream/                   # 上游 HTTP 客户端
```

## 新增适配能力

如果发现上游对某个 Anthropic 能力适配不全,按这个模式加:

1. **排查**:用 `scripts/` 抓包脚本,对比上游 vs deepseek/智谱官方的真实请求/响应,定位差异
2. **请求侧改写**(如需):在 `internal/proxy/rewriter.go` 加改写函数,遵循透传优先(只动需要改的字段)
3. **响应侧拦截**(如需):在 `internal/proxy/stream_filter.go` 加拦截逻辑
4. **测试**:纯函数单测 + httptest 集成测试 + 端到端真实链路验证
5. **文档**:更新 `docs/protocol-adaptation.md`

## 提交规范

- 代码:遵循 `gofmt` + `go vet`
- 提交信息:中文描述,清晰说明改了什么、为什么
- 不提交 `.env`、`captures/`、二进制
