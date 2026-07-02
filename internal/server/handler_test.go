package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/godkey/kdx-anthropic-bridge/internal/config"
)

// newTestServer 起一个带假上游的测试 Server。
// upstreamHandler 处理假上游请求,返回模拟响应。
// 返回代理 Server 和假上游地址(已注入)。
func newTestServer(t *testing.T, upstreamHandler http.HandlerFunc) *Server {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)

	cfg := &config.Config{
		ProxyHost:       "127.0.0.1",
		ProxyPort:       0, // 不实际监听,用 httptest
		ProxyKey:        "test-proxy-key",
		UpstreamBaseURL: up.URL,
		UpstreamAPIKey:  "fake-upstream-key",
	}
	s := New(cfg)
	// 注入假上游 client(覆盖默认 http.Client,用 httptest 的)
	s.upstream.HTTP = up.Client()
	// httptest 上游是 HTTP,基址已含 http://127.0.0.1:port
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
