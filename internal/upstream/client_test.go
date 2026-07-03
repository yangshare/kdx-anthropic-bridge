package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newClient 构造指向假上游的测试 Client。
func newClient(t *testing.T, upURL string, maxRetries int, interval time.Duration) *Client {
	t.Helper()
	return &Client{
		BaseURL:       upURL,
		APIKey:        "fake-key",
		HTTP:          &http.Client{Timeout: 10 * time.Second},
		MaxRetries:    maxRetries,
		RetryInterval: interval,
	}
}

// TestForward_retryThenSuccess 首次 503,第二次 200,验证重试一次后成功。
func TestForward_retryThenSuccess(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	t.Cleanup(up.Close)

	c := newClient(t, up.URL, 3, time.Millisecond)
	resp, err := c.Forward(http.MethodPost, "/v1/messages", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2 (1 fail + 1 success)", calls)
	}
}

// TestForward_retryExhausted 连续 503 达上限,验证重试 MaxRetries 次后返回最后一次 503。
func TestForward_retryExhausted(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(up.Close)

	c := newClient(t, up.URL, 3, time.Millisecond)
	resp, err := c.Forward(http.MethodPost, "/v1/messages", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("Forward err: %v, want response with 503", err)
	}
	defer resp.Body.Close()

	// 首次 + 3 次重试 = 4 次
	if atomic.LoadInt32(&calls) != 4 {
		t.Errorf("calls = %d, want 4", calls)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestForward_nonRetryableStatus 首次 500,不重试,立即返回。
func TestForward_nonRetryableStatus(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(up.Close)

	c := newClient(t, up.URL, 3, time.Millisecond)
	resp, err := c.Forward(http.MethodPost, "/v1/messages", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	defer resp.Body.Close()

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (500 should not retry)", calls)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// TestForward_429Retries 429 也重试。
func TestForward_429Retries(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	c := newClient(t, up.URL, 3, time.Millisecond)
	resp, err := c.Forward(http.MethodPost, "/v1/messages", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// TestForward_bodyReplayed 重试时上游每次都收到完整请求体。
func TestForward_bodyReplayed(t *testing.T) {
	var received [][]byte
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = append(received, b)
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	body := []byte(`{"query":"hello world"}`)
	c := newClient(t, up.URL, 5, time.Millisecond)
	resp, err := c.Forward(http.MethodPost, "/v1/messages", body, nil)
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	defer resp.Body.Close()

	if int(atomic.LoadInt32(&calls)) != len(received) {
		t.Fatalf("calls=%d but received %d bodies", calls, len(received))
	}
	for i, b := range received {
		if string(b) != string(body) {
			t.Errorf("attempt %d body = %q, want %q (retry must replay body)", i, b, body)
		}
	}
}

// TestForward_maxRetriesZero 配 MaxRetries=0 不重试,503 直接返回。
func TestForward_maxRetriesZero(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(up.Close)

	c := newClient(t, up.URL, 0, time.Millisecond)
	resp, err := c.Forward(http.MethodPost, "/v1/messages", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	defer resp.Body.Close()

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", calls)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestForward_headerTimeoutRetries 上游挂起(不回响应头)时,
// ResponseHeaderTimeout 触发,验证会重试并在恢复后成功。
// 模拟"上游间歇性挂起":第一次请求故意 block,第二次正常返回。
func TestForward_headerTimeoutRetries(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// 故意不写 Header 也不返回,触发 ResponseHeaderTimeout
			time.Sleep(3 * time.Second)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	t.Cleanup(up.Close)

	// 用带 ResponseHeaderTimeout 的 transport,1 秒等响应头
	transport := &http.Transport{
		ResponseHeaderTimeout: 1 * time.Second,
	}
	c := &Client{
		BaseURL:       up.URL,
		APIKey:        "fake-key",
		HTTP:          &http.Client{Transport: transport},
		MaxRetries:    2,
		RetryInterval: 100 * time.Millisecond,
	}

	resp, err := c.Forward(http.MethodPost, "/v1/messages", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (after retry)", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2 (1 hang + 1 success)", calls)
	}
}

// TestForward_injectsAuthKey 验证转发时注入上游鉴权头,替换为上游 key。
func TestForward_injectsAuthKey(t *testing.T) {
	var gotAuth, gotXKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	// 传入带 Claude Code 侧 proxy key 的鉴权头,应被替换
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer proxy-side-key")
	hdr.Set("x-api-key", "proxy-side-key")

	c := newClient(t, up.URL, 0, 0)
	resp, err := c.Forward(http.MethodPost, "/v1/messages", []byte(`{}`), hdr)
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	defer resp.Body.Close()

	if gotAuth != "Bearer fake-key" {
		t.Errorf("Authorization = %q, want Bearer fake-key", gotAuth)
	}
	if gotXKey != "fake-key" {
		t.Errorf("x-api-key = %q, want fake-key", gotXKey)
	}
}
