package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/godkey/kdx-anthropic-bridge/internal/config"
)

// newTestServer 起一个带假上游的测试 Server(单平台,profile 全开)。
// upstreamHandler 处理假上游请求,返回模拟响应。
func newTestServer(t *testing.T, upstreamHandler http.HandlerFunc) *Server {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0}, // 不实际监听,用 httptest
		GoogleSearch: config.GoogleSearchConfig{
			Timeout: 15,
			Limit:   5,
		},
		Platforms: []config.Platform{
			{
				Name:     "test",
				ProxyKey: "test-proxy-key",
				BaseURL:  up.URL,
				APIKey:   "fake-upstream-key",
				Profile:  "default",
			},
		},
		Profiles: map[string]config.Profile{
			"default": {
				RewriteThinking:  true,
				RewriteWebSearch: true,
				HeaderTimeout:    config.Duration(30 * time.Second),
				Parallel:         1,
			},
		},
	}
	s := New(cfg)
	// 注入假上游 client(覆盖默认 transport,用 httptest 的)
	s.byProxyKey["test-proxy-key"].client.HTTP = up.Client()
	return s
}

// doProxy 向代理 Server 发一个请求,返回响应。
func doProxy(s *Server, method, path string, body string, authKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if authKey != "" {
		req.Header.Set("Authorization", "Bearer "+authKey)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestHandler_messagesRewritesThinking(t *testing.T) {
	// 假上游:收请求,断言 thinking 被改写,返回简单 SSE
	var receivedBody []byte
	up := func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\ndata: {}\n")
	}
	s := newTestServer(t, up)

	body := `{"model":"xopglm52","thinking":{"type":"adaptive","display":"omitted"},"messages":[]}`
	rec := doProxy(s, "POST", "/v1/messages?beta=true", body, "test-proxy-key")

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// 上游收到的 thinking 应该已被改写
	got := string(receivedBody)
	if !strings.Contains(got, `"type":"enabled"`) {
		t.Errorf("upstream did not receive rewritten thinking\ngot: %s", got)
	}
	if strings.Contains(got, "adaptive") {
		t.Errorf("adaptive should be removed\ngot: %s", got)
	}
	if strings.Contains(got, "omitted") {
		t.Errorf("display should be removed\ngot: %s", got)
	}
}

func TestHandler_passthroughResponseBytes(t *testing.T) {
	// 响应逐字节透传
	wantResp := "event: message_start\ndata: {\"x\":1}\n\nevent: message_stop\ndata: {}\n"
	up := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Custom", "custom-val")
		w.WriteHeader(200)
		io.WriteString(w, wantResp)
	}
	s := newTestServer(t, up)

	rec := doProxy(s, "POST", "/v1/messages", `{"thinking":{"type":"enabled"}}`, "test-proxy-key")

	if rec.Body.String() != wantResp {
		t.Errorf("response not passed through verbatim\ngot:  %q\nwant: %q", rec.Body.String(), wantResp)
	}
	if rec.Header().Get("X-Custom") != "custom-val" {
		t.Errorf("custom header lost: %v", rec.Header())
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("content-type lost: %v", rec.Header())
	}
}

func TestHandler_unauthorizedWrongKey(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called with wrong key")
	}
	s := newTestServer(t, up)

	rec := doProxy(s, "POST", "/v1/messages", `{}`, "wrong-key")
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandler_unauthorizedNoKey(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called without key")
	}
	s := newTestServer(t, up)

	rec := doProxy(s, "POST", "/v1/messages", `{}`, "")
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandler_xApiKeyAuth(t *testing.T) {
	// x-api-key 头也能鉴权
	up := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}
	s := newTestServer(t, up)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("x-api-key auth failed: status=%d", rec.Code)
	}
}

func TestHandler_nonMessagesPathPassthrough(t *testing.T) {
	// /v1/messages/count_tokens 不改写请求体,原样透传
	var receivedBody []byte
	up := func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		io.WriteString(w, `{"input_tokens":5}`)
	}
	s := newTestServer(t, up)

	// 带个 adaptive,验证 count_tokens 不改它
	body := `{"model":"x","thinking":{"type":"adaptive"},"messages":[]}`
	rec := doProxy(s, "POST", "/v1/messages/count_tokens", body, "test-proxy-key")

	if rec.Code != 200 {
		t.Errorf("status=%d", rec.Code)
	}
	// 请求体原样透传,adaptive 保留
	if !bytes.Contains(receivedBody, []byte("adaptive")) {
		t.Errorf("count_tokens body should pass through unchanged\ngot: %s", receivedBody)
	}
}

func TestHandler_upstreamInjectsAuthKey(t *testing.T) {
	// 验证代理用上游 key 替换鉴权头,不透传 Claude Code 侧的 proxy key
	var gotAuth, gotXKey string
	up := func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXKey = r.Header.Get("x-api-key")
		w.WriteHeader(200)
	}
	s := newTestServer(t, up)

	doProxy(s, "POST", "/v1/messages", `{}`, "test-proxy-key")

	if !strings.Contains(gotAuth, "fake-upstream-key") {
		t.Errorf("upstream Authorization = %q, want fake-upstream-key", gotAuth)
	}
	if gotXKey != "fake-upstream-key" {
		t.Errorf("upstream x-api-key = %q, want fake-upstream-key", gotXKey)
	}
	// 不应包含 proxy 侧的 key
	if strings.Contains(gotAuth, "test-proxy-key") {
		t.Errorf("proxy key leaked to upstream: %s", gotAuth)
	}
}

// TestHandler_multiPlatformRouting 两个平台(kdx 改写 / anthropic 透传)+ 未知 key 401。
// 验证不同 proxy key 路由到不同上游,且改写按 profile 差异生效。
func TestHandler_multiPlatformRouting(t *testing.T) {
	var kdxBody, anthropicBody []byte
	kdxUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kdxBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\ndata: {}\n")
	}))
	t.Cleanup(kdxUp.Close)
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\ndata: {}\n")
	}))
	t.Cleanup(anthropicUp.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Platforms: []config.Platform{
			{Name: "kdx", ProxyKey: "token-kdx", BaseURL: kdxUp.URL, APIKey: "kdx-key", Profile: "keding"},
			{Name: "anthropic", ProxyKey: "token-anthropic", BaseURL: anthropicUp.URL, APIKey: "ant-key", Profile: "official"},
		},
		Profiles: map[string]config.Profile{
			"keding":   {RewriteThinking: true, RewriteWebSearch: true, HeaderTimeout: config.Duration(30 * time.Second)},
			"official": {RewriteThinking: false, RewriteWebSearch: false, HeaderTimeout: config.Duration(60 * time.Second)},
		},
	}
	s := New(cfg)
	s.byProxyKey["token-kdx"].client.HTTP = kdxUp.Client()
	s.byProxyKey["token-anthropic"].client.HTTP = anthropicUp.Client()

	body := `{"thinking":{"type":"adaptive"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`

	// kdx 平台:thinking + web_search 都改写
	doProxy(s, "POST", "/v1/messages", body, "token-kdx")
	if !strings.Contains(string(kdxBody), `"type":"enabled"`) {
		t.Errorf("kdx should rewrite thinking\ngot: %s", kdxBody)
	}
	if strings.Contains(string(kdxBody), "adaptive") {
		t.Errorf("kdx should remove adaptive\ngot: %s", kdxBody)
	}
	if strings.Contains(string(kdxBody), "web_search_20250305") {
		t.Errorf("kdx should rewrite web_search tool\ngot: %s", kdxBody)
	}

	// anthropic 平台:全透传
	doProxy(s, "POST", "/v1/messages", body, "token-anthropic")
	if !strings.Contains(string(anthropicBody), "adaptive") {
		t.Errorf("anthropic should pass thinking through\ngot: %s", anthropicBody)
	}
	if !strings.Contains(string(anthropicBody), "web_search_20250305") {
		t.Errorf("anthropic should keep web_search_20250305\ngot: %s", anthropicBody)
	}

	// 未知 key:401
	rec := doProxy(s, "POST", "/v1/messages", body, "token-unknown")
	if rec.Code != 401 {
		t.Errorf("unknown key status = %d, want 401", rec.Code)
	}
}

// TestHandler_webSearchInterceptOnlyWhenProfileEnabled kdx 开了 web_search 改写,
// 请求带 web_search 工具时响应走拦截路径(此处 searcher 为 nil,验证不 panic 即可)。
func TestHandler_webSearchInterceptOnlyWhenProfileEnabled(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\ndata: {}\n")
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Platforms: []config.Platform{
			{Name: "official", ProxyKey: "tok", BaseURL: up.URL, APIKey: "k", Profile: "official"},
		},
		Profiles: map[string]config.Profile{
			"official": {RewriteWebSearch: false, HeaderTimeout: config.Duration(30 * time.Second)},
		},
	}
	s := New(cfg)
	s.byProxyKey["tok"].client.HTTP = up.Client()

	// official profile 关了 web_search 改写:即使请求带 web_search_20250305,
	// 也不改写、不触发响应拦截(searcher 也为 nil),原样透传,不 panic。
	body := `{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`
	rec := doProxy(s, "POST", "/v1/messages", body, "tok")
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
