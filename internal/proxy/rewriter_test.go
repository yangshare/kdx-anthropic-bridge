package proxy

import (
	"encoding/json"
	"testing"
)

// parseHelper 解析 JSON 为 map,辅助测试断言。
func parseHelper(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	return m
}

func TestRewriteRequest_adaptiveToEnabled(t *testing.T) {
	// 核心用例:adaptive + display + budget_tokens -> 只剩 enabled
	in := []byte(`{"model":"xopglm52","max_tokens":1024,"thinking":{"type":"adaptive","display":"omitted","budget_tokens":8000},"messages":[]}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("RewriteRequest error: %v", err)
	}
	if r.HasWebSearch {
		t.Errorf("HasWebSearch should be false")
	}
	m := parseHelper(t, r.Body)
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking not a map: %v", m["thinking"])
	}
	if th["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", th["type"])
	}
	if _, exists := th["display"]; exists {
		t.Errorf("display should be dropped, got %v", th["display"])
	}
	if _, exists := th["budget_tokens"]; exists {
		t.Errorf("budget_tokens should be dropped, got %v", th["budget_tokens"])
	}
	if m["model"] != "xopglm52" {
		t.Errorf("model changed: %v", m["model"])
	}
	if m["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens changed: %v", m["max_tokens"])
	}
}

func TestRewriteRequest_alreadyEnabled_unchanged(t *testing.T) {
	in := []byte(`{"thinking":{"type":"enabled","budget_tokens":2000}}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(r.Body) != string(in) {
		t.Errorf("already enabled should return original body\ngot:  %s\nwant: %s", r.Body, in)
	}
}

func TestRewriteRequest_noThinking_unchanged(t *testing.T) {
	in := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"hi"}]}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(r.Body) != string(in) {
		t.Errorf("no thinking should return original body\ngot:  %s\nwant: %s", r.Body, in)
	}
}

func TestRewriteRequest_nonAdaptiveType_unchanged(t *testing.T) {
	in := []byte(`{"thinking":{"type":"disabled"}}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(r.Body) != string(in) {
		t.Errorf("non-adaptive type should return original body\ngot:  %s\nwant: %s", r.Body, in)
	}
}

func TestRewriteRequest_thinkingNotMap_unchanged(t *testing.T) {
	in := []byte(`{"thinking":"enabled"}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(r.Body) != string(in) {
		t.Errorf("thinking not map should return original body\ngot:  %s\nwant: %s", r.Body, in)
	}
}

func TestRewriteRequest_emptyBody_unchanged(t *testing.T) {
	r, err := RewriteRequest([]byte{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(r.Body) != 0 {
		t.Errorf("empty body should stay empty, got %d bytes", len(r.Body))
	}
}

func TestRewriteRequest_invalidJSON_error(t *testing.T) {
	in := []byte(`{not valid json}`)
	_, err := RewriteRequest(in)
	if err == nil {
		t.Fatal("invalid JSON should return error")
	}
}

func TestRewriteRequest_preservesUnknownFields(t *testing.T) {
	in := []byte(`{
		"model":"xopglm52",
		"thinking":{"type":"adaptive","display":"omitted"},
		"output_config":{"effort":"high"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"tools":[{"name":"Bash","input_schema":{}}],
		"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]
	}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	m := parseHelper(t, r.Body)
	if th, _ := m["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Errorf("thinking not rewritten: %v", m["thinking"])
	}
	if oc, _ := m["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config lost: %v", m["output_config"])
	}
	if _, ok := m["context_management"]; !ok {
		t.Error("context_management lost")
	}
	if tools, _ := m["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools count changed: %v", m["tools"])
	}
	if sys, _ := m["system"].([]any); len(sys) != 1 {
		t.Errorf("system count changed: %v", m["system"])
	}
}

// ===== web_search 改写测试 =====

func TestRewriteRequest_webSearchToolRewritten(t *testing.T) {
	// web_search_20250305 -> 普通 function tool(带 query input_schema)
	in := []byte(`{"model":"xopglm52","tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],"messages":[]}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !r.HasWebSearch {
		t.Errorf("HasWebSearch should be true")
	}
	m := parseHelper(t, r.Body)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools lost: %v", m["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] == "web_search_20250305" {
		t.Errorf("web_search_20250305 should be replaced")
	}
	if tool["name"] != "web_search" {
		t.Errorf("name = %v, want web_search", tool["name"])
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema == nil {
		t.Fatalf("input_schema missing")
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Errorf("input_schema should have query property: %v", schema)
	}
}

func TestRewriteRequest_webSearchMixedWithOtherTools(t *testing.T) {
	// web_search 和其他工具混合,只改 web_search,其他不动
	in := []byte(`{"tools":[
		{"name":"Bash","description":"run","input_schema":{}},
		{"type":"web_search_20250305","name":"web_search","max_uses":8},
		{"name":"Read","input_schema":{}}
	]}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !r.HasWebSearch {
		t.Errorf("HasWebSearch should be true")
	}
	m := parseHelper(t, r.Body)
	tools, _ := m["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools count changed: %d", len(tools))
	}
	// 第1、3个不变
	if tools[0].(map[string]any)["name"] != "Bash" {
		t.Errorf("Bash changed")
	}
	if tools[2].(map[string]any)["name"] != "Read" {
		t.Errorf("Read changed")
	}
	// 第2个被改写
	ws := tools[1].(map[string]any)
	if ws["type"] == "web_search_20250305" {
		t.Errorf("web_search not rewritten")
	}
	if ws["name"] != "web_search" {
		t.Errorf("name = %v", ws["name"])
	}
}

func TestRewriteRequest_noWebSearch_hasWebSearchFalse(t *testing.T) {
	in := []byte(`{"tools":[{"name":"Bash","input_schema":{}}]}`)
	r, err := RewriteRequest(in)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if r.HasWebSearch {
		t.Errorf("HasWebSearch should be false when no web_search tool")
	}
}
