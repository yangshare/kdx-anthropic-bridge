// Package upstream 实现对科大 Anthropic 端点的流式转发。
//
// 只负责:把改写后的请求体原样 POST 到上游,流式回传响应。
// 不做任何业务改写,响应逐字节透传。
package upstream

import (
	"fmt"
	"io"
	"net/http"
)

// Client 科大上游客户端。
type Client struct {
	// BaseURL 上游基址,如 https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic
	BaseURL string
	// APIKey 上游 key(appid:secret)
	APIKey string
	// HTTP 底层 HTTP 客户端(可注入便于测试)
	HTTP *http.Client
}

// Forward 将请求体转发到上游,返回上游响应。
// 调用方负责读取并流式回传 resp.Body,以及关闭 resp.Body。
//
// path 是 Claude Code 请求的原始路径(含 query),如 /v1/messages?beta=true
// headers 是需要透传的请求头(鉴权头会被替换为上游 key)。
func (c *Client) Forward(method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}

	url := c.BaseURL + path
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

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: do request: %w", err)
	}
	return resp, nil
}

// isAuthHeader 判断是否为鉴权相关头(转发时要替换,不能透传 Claude Code 侧的)。
func isAuthHeader(h string) bool {
	switch http.CanonicalHeaderKey(h) {
	case "Authorization", "X-Api-Key":
		return true
	}
	return false
}
