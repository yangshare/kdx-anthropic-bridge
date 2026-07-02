package proxy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/godkey/kdx-anthropic-bridge/internal/search"
)

// fakeSearcher 假搜索执行器,返回固定结果,便于断言。
type fakeSearcher struct {
	gotQuery string
	items    []search.Item
	err      error
}

func (f *fakeSearcher) Search(ctx context.Context, query string, limit int) ([]search.Item, error) {
	f.gotQuery = query
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

// 模拟科大返回的 SSE:web_search tool_use 带 query input
const kdWebSearchSSE = `event: message_start
data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"index":0,"content_block":{"type":"text","text":""},"type":"content_block_start"}

event: content_block_delta
data: {"index":0,"delta":{"text":"let me search","type":"text_delta"},"type":"content_block_delta"}

event: content_block_stop
data: {"index":0,"type":"content_block_stop"}

event: content_block_start
data: {"index":1,"content_block":{"id":"call_abc","input":{},"name":"web_search","type":"tool_use"},"type":"content_block_start"}

event: content_block_delta
data: {"index":1,"delta":{"partial_json":"{\"query\":","type":"input_json_delta"},"type":"content_block_delta"}

event: content_block_delta
data: {"index":1,"delta":{"partial_json":"\"mitmproxy latest\"}","type":"input_json_delta"},"type":"content_block_delta"}

event: content_block_stop
data: {"index":1,"type":"content_block_stop"}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":10,"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamFilter_webSearchRewritten(t *testing.T) {
	fs := &fakeSearcher{
		items: []search.Item{
			{Title: "Releases", URL: "https://github.com/mitmproxy/mitmproxy/releases"},
			{Title: "Downloads", URL: "https://mitmproxy.org/downloads"},
		},
	}
	f := NewStreamFilter(fs, 5)

	var out bytes.Buffer
	if err := f.FilterStream(context.Background(), &out, strings.NewReader(kdWebSearchSSE)); err != nil {
		t.Fatalf("FilterStream: %v", err)
	}

	outStr := out.String()

	// 搜索器收到了正确的 query
	if fs.gotQuery != "mitmproxy latest" {
		t.Errorf("query = %q, want 'mitmproxy latest'", fs.gotQuery)
	}

	// text block(index 0)原样透传
	if !strings.Contains(outStr, "let me search") {
		t.Errorf("text block not passed through")
	}

	// web_search tool_use 被替换成 server_tool_use
	if !strings.Contains(outStr, `"type":"server_tool_use"`) {
		t.Errorf("server_tool_use not emitted")
	}
	if !strings.Contains(outStr, `"name":"web_search"`) {
		t.Errorf("server_tool_use name missing")
	}

	// web_search_tool_result 含真实 title/url
	if !strings.Contains(outStr, `"type":"web_search_tool_result"`) {
		t.Errorf("web_search_tool_result not emitted")
	}
	if !strings.Contains(outStr, "github.com/mitmproxy/mitmproxy/releases") {
		t.Errorf("search result url missing in output")
	}
	if !strings.Contains(outStr, `"title":"Releases"`) {
		t.Errorf("search result title missing in output")
	}

	// 改写后的 server_tool_use 应带 input_json_delta 流式输出 query
	if !strings.Contains(outStr, "input_json_delta") {
		t.Errorf("rewritten server_tool_use should emit input_json_delta with query")
	}
	// query 被切成多片,至少有一部分出现在 partial_json 里(query 词被切可能不全)
	if !strings.Contains(outStr, "mitm") {
		t.Errorf("query fragment should appear in input_json_delta")
	}

	// stop_reason 应透传
	if !strings.Contains(outStr, "tool_use") {
		t.Errorf("stop_reason not passed through")
	}
}

func TestStreamFilter_noWebSearch_passthrough(t *testing.T) {
	// 没有web_search 的普通响应,原样透传
	fs := &fakeSearcher{}
	f := NewStreamFilter(fs, 5)

	sse := `event: message_start
data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[],"stop_reason":null,"usage":{}}}

event: content_block_start
data: {"index":0,"content_block":{"text":"","type":"text"},"type":"content_block_start"}

event: content_block_delta
data: {"index":0,"delta":{"text":"hello","type":"text_delta"},"type":"content_block_delta"}

event: content_block_stop
data: {"index":0,"type":"content_block_stop"}

event: message_stop
data: {"type":"message_stop"}

`
	var out bytes.Buffer
	if err := f.FilterStream(context.Background(), &out, strings.NewReader(sse)); err != nil {
		t.Fatalf("FilterStream: %v", err)
	}

	if fs.gotQuery != "" {
		t.Errorf("searcher should not be called, got query=%q", fs.gotQuery)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("text not passed through")
	}
	if strings.Contains(out.String(), "server_tool_use") {
		t.Errorf("should not emit server_tool_use when no web_search")
	}
}

func TestStreamFilter_otherToolUse_passthrough(t *testing.T) {
	// 非 web_search 的 tool_use(如 Bash)原样透传,不拦截
	fs := &fakeSearcher{}
	f := NewStreamFilter(fs, 5)

	sse := `event: content_block_start
data: {"index":0,"content_block":{"id":"call_1","input":{},"name":"Bash","type":"tool_use"},"type":"content_block_start"}

event: content_block_delta
data: {"index":0,"delta":{"partial_json":"{\"command\":\"ls\"}","type":"input_json_delta"},"type":"content_block_delta"}

event: content_block_stop
data: {"index":0,"type":"content_block_stop"}

`
	var out bytes.Buffer
	if err := f.FilterStream(context.Background(), &out, strings.NewReader(sse)); err != nil {
		t.Fatalf("FilterStream: %v", err)
	}

	if fs.gotQuery != "" {
		t.Errorf("searcher should not be called for Bash tool")
	}
	if !strings.Contains(out.String(), `"name":"Bash"`) {
		t.Errorf("Bash tool_use should pass through")
	}
	if !strings.Contains(out.String(), "input_json_delta") {
		t.Errorf("Bash input_json_delta should pass through")
	}
}

func TestStreamFilter_searchError_outputsErrorResult(t *testing.T) {
	// 搜索失败时输出 web_search_tool_result_error
	fs := &fakeSearcher{err: errFake}
	f := NewStreamFilter(fs, 5)

	var out bytes.Buffer
	if err := f.FilterStream(context.Background(), &out, strings.NewReader(kdWebSearchSSE)); err != nil {
		t.Fatalf("FilterStream: %v", err)
	}

	if !strings.Contains(out.String(), "web_search_tool_result_error") {
		t.Errorf("should emit error result on search failure:\n%s", out.String())
	}
}

// errFake 测试用错误
var errFake = bytesError("search failed")

type bytesError string

func (e bytesError) Error() string { return string(e) }

func TestStreamFilter_emptyQuery(t *testing.T) {
	// web_search tool_use 但 input 为空(科大常见情况)
	fs := &fakeSearcher{items: nil}
	f := NewStreamFilter(fs, 5)

	sse := `event: content_block_start
data: {"index":0,"content_block":{"id":"call_x","input":{},"name":"web_search","type":"tool_use"},"type":"content_block_start"}

event: content_block_stop
data: {"index":0,"type":"content_block_stop"}

`
	var out bytes.Buffer
	if err := f.FilterStream(context.Background(), &out, strings.NewReader(sse)); err != nil {
		t.Fatalf("FilterStream: %v", err)
	}
	// 空 query 也应正常处理(搜索可能失败,但流程不崩)
	if fs.gotQuery != "" {
		t.Errorf("empty query expected, got %q", fs.gotQuery)
	}
}
