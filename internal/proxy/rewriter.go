// Package proxy 实现请求改写与转发编排。
//
// 本包做协议层的字段改写,不做业务判断。当前职责:
//   - 将 Claude Code 发出的 thinking.type=adaptive 改写为科大端点认识的 enabled
//   - 将服务端 web_search_20250305 工具改写为普通 function tool,
//     使模型发起带 query 的 tool_use(而非空 input),供代理拦截后自行搜索
package proxy

import (
	"encoding/json"
	"fmt"
)

// webSearchType 科大不支持的服务端 web_search 工具类型标识
const webSearchType = "web_search_20250305"

// RewriteResult RewriteRequest 的返回,带改写后的 body 和元信息。
type RewriteResult struct {
	Body []byte
	// HasWebSearch 请求里是否含 web_search 工具(响应侧据此决定是否拦截)
	HasWebSearch bool
}

// RewriteRequest 改写 Anthropic /v1/messages 请求体。
//
// 改写规则(其他字段一律透传):
//   - thinking.type == "adaptive"  ->  {"type":"enabled"}
//   - tools 里的 web_search_20250305  ->  普通 function tool(带 query input_schema)
//
// 没有需要改的字段时返回原始 body,避免重新序列化改变字节顺序。
func RewriteRequest(body []byte) (*RewriteResult, error) {
	if len(body) == 0 {
		return &RewriteResult{Body: body}, nil
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("rewriter: parse request body: %w", err)
	}

	changed := rewriteThinking(req)
	hasWS := rewriteWebSearchTools(req)
	if !changed && !hasWS {
		return &RewriteResult{Body: body, HasWebSearch: hasWS}, nil
	}

	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("rewriter: marshal request body: %w", err)
	}
	return &RewriteResult{Body: out, HasWebSearch: hasWS}, nil
}

// rewriteThinking 就地改写 req 里的 thinking 字段,返回是否发生改动。
func rewriteThinking(req map[string]any) bool {
	thinking, ok := req["thinking"].(map[string]any)
	if !ok {
		return false
	}
	if thinking["type"] != "adaptive" {
		return false
	}
	// adaptive -> enabled,丢掉 display / budget_tokens 等子字段
	// (科大不认 budget_tokens,display 是 Claude Code 本地标记)
	req["thinking"] = map[string]any{"type": "enabled"}
	return true
}

// rewriteWebSearchTools 把 tools 数组里的 web_search_20250305 服务端工具
// 改写成普通 function tool(带 query input_schema)。
//
// 科大不支持服务端 web_search,会把 web_search_20250305 退化成空 input 的
// 普通 tool_use(死循环)。改成带 query input_schema 的 function tool 后,
// 模型会发起带 query 的 tool_use,代理在响应侧拦截并自行搜索。
//
// 返回原请求是否含 web_search 工具(无论是否已改写)。
func rewriteWebSearchTools(req map[string]any) bool {
	tools, ok := req["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}

	found := false
	for i, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tool["type"] != webSearchType {
			continue
		}
		found = true
		tools[i] = webSearchFunctionTool()
	}
	return found
}

// webSearchFunctionTool 构造普通 function tool 定义,替代服务端 web_search。
//
// input_schema 让模型填 query 字段。代理响应侧据此拿到 query 去搜索。
func webSearchFunctionTool() map[string]any {
	return map[string]any{
		"name":        "web_search",
		"description": "Search the web. Returns a list of results with title and url.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query",
				},
			},
			"required": []string{"query"},
		},
	}
}
