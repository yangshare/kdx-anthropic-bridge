// Package upstream 实现对科大 Anthropic 端点的流式转发。
//
// 职责:把改写后的请求体原样 POST 到上游,流式回传响应。
// 遇上游 502/503/429 时按固定间隔重试(可配置次数),其他状态码立即返回。
// 不做任何业务改写,成功响应逐字节透传。
package upstream

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

// retryableStatus 上游返回这些状态码时重试:网关类瞬时错误。
var retryableStatus = map[int]bool{
	http.StatusBadGateway:         true, // 502
	http.StatusServiceUnavailable: true, // 503
	http.StatusTooManyRequests:    true, // 429
}

// Client 科大上游客户端。
type Client struct {
	// BaseURL 上游基址,如 https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic
	BaseURL string
	// APIKey 上游 key(appid:secret)
	APIKey string
	// HTTP 底层 HTTP 客户端(可注入便于测试)
	HTTP *http.Client
	// MaxRetries 上游 502/503/429 时的最大重试次数(不含首次)。0 = 不重试。
	MaxRetries int
	// RetryInterval 重试间隔。0 = 不等待。
	RetryInterval time.Duration
}

// Forward 将请求体转发到上游,返回上游响应。
// 调用方负责读取并流式回传 resp.Body,以及关闭 resp.Body。
//
// body 以 []byte 传入,是为了让重试能重放请求体(http.NewRequest 读走
// io.Reader 后无法重用)。nil 表示无请求体(GET 等)。
//
// path 是 Claude Code 请求的原始路径(含 query),如 /v1/messages?beta=true
// headers 是需要透传的请求头(鉴权头会被替换为上游 key)。
//
// 重试策略:上游返回 502/503/429 且重试次数未达 MaxRetries 时,关闭响应体,
// 等待 RetryInterval 后重发。达到上限后返回最后一次响应(含错误状态码),
// 由调用方透传给下游。网络错误同样重试(上游不可达视为瞬时故障)。
func (c *Client) Forward(method, path string, body []byte, headers http.Header) (*http.Response, error) {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}

	url := c.BaseURL + path
	attempts := c.MaxRetries + 1 // 首次 + 重试次数

	var lastNetErr error

	for i := 0; i < attempts; i++ {
		// 每次重试重新构造请求体 Reader(body 可重放)
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		resp, err := c.doOnce(method, url, bodyReader, headers)

		if err != nil {
			// 网络层错误:记录,重试
			lastNetErr = err
			if i < attempts-1 && c.shouldSleep() {
				time.Sleep(c.RetryInterval)
			}
			continue
		}

		// 可重试状态码且仍有重试机会:关 body,等待后重试
		if retryableStatus[resp.StatusCode] && i < attempts-1 {
			resp.Body.Close()
			if c.shouldSleep() {
				time.Sleep(c.RetryInterval)
			}
			continue
		}

		// 成功 / 非重试状态码 / 最后一次:直接返回(resp.Body 未被关闭,可读)
		return resp, nil
	}

	// 重试耗尽。走到这说明全程要么网络错误,要么可重试状态码,且已达上限。
	// 上一次循环若是可重试状态码,在循环内已 return(最后一次不进 if 分支)。
	// 能走到这,只可能是全程网络错误。
	if lastNetErr != nil {
		return nil, fmt.Errorf("upstream: request failed after %d attempts: %w", attempts, lastNetErr)
	}
	// 理论不可达:attempts>=1,循环内必 return 或记录 lastNetErr
	return nil, fmt.Errorf("upstream: no attempt completed")
}

// doOnce 发一次请求,构造 header 并替换鉴权头。返回响应或错误。
func (c *Client) doOnce(method, url string, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("upstream: build request: %w", err)
	}

	// 透传非鉴权头
	for k, vs := range headers {
		// 跳过 Claude Code 侧的鉴权头和 Host,用上游的
		if isAuthHeader(k) || k == "Host" || k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// 注入上游鉴权头(同时给 Authorization 和 x-api-key,科大两者都认)
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("x-api-key", c.APIKey)

	return c.HTTP.Do(req)
}

// shouldSleep 是否需要在重试间等待。配置了正数间隔才睡。
func (c *Client) shouldSleep() bool {
	return c.RetryInterval > 0
}

// isAuthHeader 判断是否为鉴权相关头(转发时要替换,不能透传 Claude Code 侧的)。
func isAuthHeader(h string) bool {
	switch http.CanonicalHeaderKey(h) {
	case "Authorization", "X-Api-Key":
		return true
	}
	return false
}
