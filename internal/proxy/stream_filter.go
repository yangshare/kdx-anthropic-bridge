// Package proxy 响应侧流式过滤器:拦截 web_search tool_use,改写为服务端搜索结果。
//
// 科大不支持服务端 web_search,模型发起的是普通 tool_use(name=web_search)。
// 代理在响应流里检测到这种 block 时:
//   - 暂停向下游转发该 block 的所有事件
//   - 收集 input(从 input_json_delta 拼出 query)
//   - 调用内置谷歌搜索
//   - 向下游改写为 server_tool_use + web_search_tool_result 两个 block(同 index)
//   - 其他 block 一律透传
package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/godkey/kdx-anthropic-bridge/internal/search"
)

// webSearchToolName 代理要拦截的 tool_use 名称
const webSearchToolName = "web_search"

// WebSearchExecutor 抽象搜索能力,便于测试注入假实现。
type WebSearchExecutor interface {
	Search(ctx context.Context, query string, limit int) ([]search.Item, error)
}

// StreamFilter 响应流过滤器。
//
// 用法:NewStreamFilter(searcher) 构造,然后 FilterStream 把上游 SSE 流
// 逐行读、改写、写到下游。非 web_search 内容原样透传。
type StreamFilter struct {
	searcher WebSearchExecutor
	limit    int
}

// NewStreamFilter 构造过滤器。limit 是每次搜索返回的结果数。
func NewStreamFilter(searcher WebSearchExecutor, limit int) *StreamFilter {
	if limit <= 0 {
		limit = 5
	}
	return &StreamFilter{searcher: searcher, limit: limit}
}

// FilterStream 把 src(上游 SSE)过滤后写到 dst(下游)。
//
// 行级处理:SSE 以空行分隔事件,每事件含 event: 和 data: 行。
// 检测到 web_search tool_use 的 block 时,缓存该 block 的所有事件,
// 在 block 结束时调搜索并改写输出。
func (f *StreamFilter) FilterStream(ctx context.Context, dst io.Writer, src io.Reader) error {
	br := bufio.NewReader(src)
	state := &filterState{out: dst}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 流结束前,刷掉残留的缓存 block(如果有)
				if flushErr := state.flushPending(f, ctx); flushErr != nil {
					return flushErr
				}
				return nil
			}
			return fmt.Errorf("stream filter: read: %w", err)
		}

		if err := f.processLine(ctx, state, line); err != nil {
			return err
		}
	}
}

// filterState 流处理状态机。
type filterState struct {
	out io.Writer

	// 当前是否在缓存 web_search tool_use block
	capturing bool
	// 缓存的当前 block 事件(data 行的 JSON)
	pendingData []string
	// 当前捕获 block 的 index
	currentIndex int
	// 从 input_json_delta 拼出的 input JSON 片段
	inputJSONAcc []string
	// 当前 block 在 content_block_start 时的 tool_use id(用于改写)
	toolUseID string
}

// processLine 处理一行 SSE,决定透传还是缓存/改写。
func (f *StreamFilter) processLine(ctx context.Context, st *filterState, line string) error {
	// 空行:事件分隔。如果在 capturing,空行只是当前 block 的一个 delta 间隔,
	// 不结束 block(block 由 content_block_stop 结束)。透传给非 capturing 场景。
	if line == "\n" || line == "\r\n" {
		if !st.capturing {
			_, err := io.WriteString(st.out, line)
			return err
		}
		return nil
	}

	// 解析 data 行
	if strings.HasPrefix(line, "data: ") {
		data := strings.TrimPrefix(line, "data: ")
		data = strings.TrimRight(data, "\r\n")
		return f.processDataLine(ctx, st, data)
	}

	// event: 行或其他行:非 capturing 时透传
	if !st.capturing {
		_, err := io.WriteString(st.out, line)
		return err
	}
	return nil
}

// processDataLine 处理 data: 行(JSON)。
func (f *StreamFilter) processDataLine(ctx context.Context, st *filterState, data string) error {
	// 先解析出 type,判断是不是要捕获/结束的事件
	var evt map[string]any
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		// 非 JSON data,透传(非 capturing 时)
		if !st.capturing {
			_, _ = io.WriteString(st.out, "data: "+data+"\n")
		}
		return nil
	}

	evtType, _ := evt["type"].(string)

	// content_block_start:可能是 web_search tool_use 的开始
	if evtType == "content_block_start" {
		return f.handleBlockStart(st, data, evt)
	}

	// content_block_stop:如果正在 capturing,结束该 block,执行搜索+改写
	if evtType == "content_block_stop" {
		if st.capturing {
			return st.flushPending(f, ctx)
		}
		return passthroughLine(st.out, "data: "+data+"\n")
	}

	// 在 capturing 中:缓存所有 delta(拼 input JSON)
	if st.capturing {
		if evtType == "content_block_delta" {
			if delta, _ := evt["delta"].(map[string]any); delta != nil {
				if dt, _ := delta["type"].(string); dt == "input_json_delta" {
					if pj, _ := delta["partial_json"].(string); pj != "" {
						st.inputJSONAcc = append(st.inputJSONAcc, pj)
					}
				}
			}
		}
		return nil
	}

	// 非 capturing:透传(保持原行)
	return passthroughLine(st.out, "data: "+data+"\n")
}

// handleBlockStart 处理 content_block_start。
func (f *StreamFilter) handleBlockStart(st *filterState, data string, evt map[string]any) error {
	cb, _ := evt["content_block"].(map[string]any)
	if cb == nil {
		return passthroughLine(st.out, "data: "+data+"\n")
	}
	if cb["type"] != "tool_use" {
		return passthroughLine(st.out, "data: "+data+"\n")
	}
	if cb["name"] != webSearchToolName {
		return passthroughLine(st.out, "data: "+data+"\n")
	}

	// 命中 web_search tool_use:开始捕获(不透传这个 start)
	idx, _ := evt["index"].(float64)
	st.capturing = true
	st.currentIndex = int(idx)
	st.toolUseID, _ = cb["id"].(string)
	st.inputJSONAcc = nil
	st.pendingData = nil
	return nil
}

// flushPending 结束当前捕获的 web_search block,执行搜索,改写输出。
// 未在捕获状态时直接返回(EOF 时也会调此函数,需防误触发)。
func (st *filterState) flushPending(f *StreamFilter, ctx context.Context) error {
	if !st.capturing {
		return nil
	}
	defer func() {
		st.capturing = false
		st.inputJSONAcc = nil
		st.pendingData = nil
	}()

	// 拼出 input JSON,提取 query
	query := ""
	if len(st.inputJSONAcc) > 0 {
		joined := strings.Join(st.inputJSONAcc, "")
		var input map[string]any
		if err := json.Unmarshal([]byte(joined), &input); err == nil {
			if q, ok := input["query"].(string); ok {
				query = q
			}
		}
	}

	items, err := f.searcher.Search(ctx, query, f.limit)
	if err != nil {
		// 搜索失败:输出一个错误形式的 web_search_tool_result
		return writeWebSearchError(st.out, st.currentIndex, st.toolUseID, err)
	}

	// 改写:server_tool_use(index N)+ web_search_tool_result(index N+1)
	// 参照 deepseek 真实响应:两者是分开的 block,index 递增。
	// 后续科大 block 的 index 不调整(Claude Code 按顺序消费,容忍 index 跳跃)。
	if err := writeServerToolUse(st.out, st.currentIndex, st.toolUseID, query); err != nil {
		return err
	}
	if err := writeWebSearchResult(st.out, st.currentIndex+1, st.toolUseID, items); err != nil {
		return err
	}
	return nil
}

// ===== 输出辅助 =====

// passthroughLine 原样写一行(用于透传,保持上游原始格式)。
func passthroughLine(w io.Writer, line string) error {
	_, err := io.WriteString(w, line)
	return err
}

// writeEvent 写一个完整 SSE 事件:event 行 + data 行 + 空行(用于改写输出)。
// 从 data JSON 里提取 type 作为 event 名(Anthropic SSE 规范)。
func writeEvent(w io.Writer, data string) error {
	eventName := extractEventType(data)
	if eventName != "" {
		if _, err := io.WriteString(w, "event: "+eventName+"\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "data: "+data+"\n\n")
	return err
}

// extractEventType 从 data JSON 里提取 type 字段(用于 SSE event 行)。
func extractEventType(data string) string {
	var evt struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(data), &evt) == nil {
		return evt.Type
	}
	return ""
}

// writeServerToolUse 输出 server_tool_use block,带 input_json_delta 流式输出 query。
// 参照 deepseek 真实响应:Claude Code 期待 server_tool_use 后跟 input_json_delta
// 流式输出 query JSON,再 content_block_stop。
func writeServerToolUse(w io.Writer, index int, id, query string) error {
	start := fmt.Sprintf(`{"index":%d,"content_block":{"type":"server_tool_use","id":%q,"name":"web_search","input":{}},"type":"content_block_start"}`, index, id)
	if err := writeEvent(w, start); err != nil {
		return err
	}

	// 流式输出 input JSON: {"query":"<query>"}
	// 切成几片,模拟真实流式(整个 JSON 一次性也行,Claude Code 能拼)
	inputJSON, _ := json.Marshal(map[string]string{"query": query})
	deltas := splitJSONChunks(string(inputJSON))
	for _, chunk := range deltas {
		chunkJSON, _ := json.Marshal(chunk) // chunk 作为 JSON 字符串值(带引号)
		delta := fmt.Sprintf(`{"index":%d,"delta":{"type":"input_json_delta","partial_json":%s},"type":"content_block_delta"}`, index, string(chunkJSON))
		if err := writeEvent(w, delta); err != nil {
			return err
		}
	}

	stop := fmt.Sprintf(`{"index":%d,"type":"content_block_stop"}`, index)
	return writeEvent(w, stop)
}

// splitJSONChunks 把 JSON 字符串切成几片(模拟流式 input_json_delta)。
func splitJSONChunks(s string) []string {
	// 简单切:前1/3、中1/3、后1/3(至少一片)
	if len(s) <= 3 {
		return []string{s}
	}
	third := len(s) / 3
	return []string{s[:third], s[third : 2*third], s[2*third:]}
}

// writeWebSearchResult 输出 web_search_tool_result block。
func writeWebSearchResult(w io.Writer, index int, toolUseID string, items []search.Item) error {
	results := make([]map[string]any, 0, len(items))
	for _, it := range items {
		results = append(results, map[string]any{
			"type":  "web_search_result",
			"title": it.Title,
			"url":   it.URL,
		})
	}
	start := map[string]any{
		"index": index,
		"content_block": map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     results,
		},
		"type": "content_block_start",
	}
	b, _ := json.Marshal(start)
	if err := writeEvent(w, string(b)); err != nil {
		return err
	}
	stop := fmt.Sprintf(`{"index":%d,"type":"content_block_stop"}`, index)
	return writeEvent(w, stop)
}

// writeWebSearchError 搜索失败时输出错误结果块。
func writeWebSearchError(w io.Writer, index int, toolUseID string, err error) error {
	start := map[string]any{
		"index": index,
		"content_block": map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content": map[string]any{
				"type":       "web_search_tool_result_error",
				"error_code": "internal_error",
				"message":    err.Error(),
			},
		},
		"type": "content_block_start",
	}
	b, _ := json.Marshal(start)
	if err := writeEvent(w, string(b)); err != nil {
		return err
	}
	stop := fmt.Sprintf(`{"index":%d,"type":"content_block_stop"}`, index)
	return writeEvent(w, stop)
}
