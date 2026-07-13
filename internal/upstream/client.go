// Package upstream 实现对科大 Anthropic 端点的流式转发。
//
// 职责:把改写后的请求体原样 POST 到上游,流式回传响应。
// 遇上游 502/503/429 时按固定间隔重试(可配置次数),其他状态码立即返回。
// 不做任何业务改写,成功响应逐字节透传。
//
// 并发抢窗口:科大上游间歇性 503/排队,单次请求可能等十几秒才出首字节。
// 单次 attempt 内并发发 N 路(UPSTREAM_PARALLEL),谁先拿到非重试状态码就用谁,
// 其余立即取消。N=1 时退化为原串行行为。N 越大越快抢占游零星放行窗口,
// 但对上游压力翻 N 倍(共享同一 key 限流配额),反噬严重时调回 1 即退回老逻辑。
package upstream

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
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
	// Parallel 单次 attempt 内并发的请求数。<=1 退化为串行(只发一路)。
	// 用于抢占游零星放行窗口:谁先拿到非重试状态码(200 等)就用谁,其余取消。
	Parallel int
}

// raceResult 单路并发请求的结果。
type raceResult struct {
	resp *http.Response
	err  error
	// dur 本路从发起到拿到结果的耗时(用于日志)
	dur time.Duration
	// idx 本路编号(0-based),用于日志区分
	idx int
}

// Forward 将请求体转发到上游,返回上游响应。
// 调用方负责读取并流式回传 resp.Body,以及关闭 resp.Body。
//
// body 以 []byte 传入,是为了让重试与并发能重放请求体(http.NewRequest 读走
// io.Reader 后无法重用)。nil 表示无请求体(GET 等)。
//
// path 是 Claude Code 请求的原始路径(含 query),如 /v1/messages?beta=true
// headers 是需要透传的请求头(鉴权头会被替换为上游 key)。
//
// 重试策略:上游返回 502/503/429 且重试次数未达 MaxRetries 时,关闭响应体,
// 等待 RetryInterval 后重发。达到上限后返回最后一次响应(含错误状态码),
// 由调用方透传给下游。网络错误同样重试(上游不可达视为瞬时故障)。
//
// 并发抢窗口:每次 attempt 内并发 Parallel 路请求,谁先拿到非重试状态码
// 就用谁,其余立即取消并关闭响应体。全 N 路都返回可重试状态码/网络错误时,
// 才 sleep 进入下一次 attempt。
func (c *Client) Forward(method, path string, body []byte, headers http.Header) (*http.Response, error) {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}

	url := c.BaseURL + path
	attempts := c.MaxRetries + 1 // 首次 + 重试次数
	parallel := c.Parallel
	if parallel < 1 {
		parallel = 1
	}

	for i := 0; i < attempts; i++ {
		tAttempt := time.Now()
		resp, _, netErr := c.raceOnce(method, url, body, headers, parallel)
		dur := time.Since(tAttempt)

		// raceOnce 返回:
		//   resp != nil:拿到了响应(可重试码或成功码)。可重试码的 body 未关闭,可透传。
		//   resp == nil:全路网络错(netErr 非 nil),或全路可重试码(已 close)。
		//
		// 非可重试状态码:成功/4xx,直接返回。
		if resp != nil && !retryableStatus[resp.StatusCode] {
			if i > 0 || parallel > 1 {
				log.Printf("upstream attempt %d/%d (parallel=%d): status=%d after %s, returning",
					i+1, attempts, parallel, resp.StatusCode, dur)
			}
			return resp, nil
		}

		// 可重试状态码,但已是最后一次:透传最后一次响应(含 503 等)给下游,
		// 与原串行行为一致(MaxRetries=0 时 503 直接给下游)。
		if resp != nil && i >= attempts-1 {
			log.Printf("upstream attempt %d/%d (parallel=%d): status=%d after %s, returning (retries exhausted)",
				i+1, attempts, parallel, resp.StatusCode, dur)
			return resp, nil
		}

		// 还有重试机会:关掉残留 resp,记录,等待后重试
		if resp != nil {
			resp.Body.Close()
		}
		if netErr != nil {
			log.Printf("upstream attempt %d/%d (parallel=%d): net error after %s: %v",
				i+1, attempts, parallel, dur, netErr)
		} else {
			log.Printf("upstream attempt %d/%d (parallel=%d): all %d routes retryable after %s, retrying",
				i+1, attempts, parallel, parallel, dur)
		}

		if c.shouldSleep() {
			time.Sleep(c.RetryInterval)
		}
	}

	// 理论不可达:循环内 attempts>=1 必 return 或进入最后一次兜底分支
	return nil, fmt.Errorf("upstream: no attempt completed")
}

// raceOnce 在单次 attempt 内并发发 parallel 路请求抢占游零星放行窗口。
//
// 返回:
//   - resp: 若有任一路拿到非重试状态码(成功/4xx),返回第一个这样的响应(其余已取消+关闭)。
//     resp == nil 表示没有一路拿到可用响应(全可重试码或全网络错)。
//   - allRetry: 全 parallel 路都返回可重试状态码(502/503/429)。resp 必为 nil。
//   - netErr: 全 parallel 路都网络错误时的最后一个错误(用于日志/外层重试)。
//
// 语义:racing 到一个"可用响应"(非重试码)立即胜出;全路重试码或全路网络错
// 才算本次 attempt 失败,交给外层 sleep 后重试。所有可重试响应体在此关闭。
func (c *Client) raceOnce(method, url string, body []byte, headers http.Header, parallel int) (resp *http.Response, allRetry bool, netErr error) {
	if parallel <= 1 {
		// 串行快路径:复用 raceOne 单路逻辑,避免 channel 开销
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		r := c.raceOne(method, url, bodyReader, headers, 0)
		if r.err != nil {
			return nil, false, r.err
		}
		// 可重试码:不关 body,原样返回给 Forward 决定(最后一次要透传给下游)
		return r.resp, retryableStatus[r.resp.StatusCode], nil
	}

	resCh := make(chan raceResult, parallel)
	ctx := make(chan struct{}) // 任意一路拿到可用响应即 close,作为其余路的取消信号
	var closeOnce sync.Once

	for j := 0; j < parallel; j++ {
		go func(idx int) {
			var bodyReader io.Reader
			if body != nil {
				bodyReader = bytes.NewReader(body)
			}
			r := c.raceOne(method, url, bodyReader, headers, idx)
			select {
			case resCh <- r:
			case <-ctx:
				// 已有胜出者,本路是多余的响应(若拿到可用响应也要关掉)
				if r.resp != nil {
					r.resp.Body.Close()
				}
			}
		}(j)
	}

	var retryCount, netCount int
	var lastErr error
	collected := 0
	for collected < parallel {
		r := <-resCh
		collected++

		if r.err != nil {
			netCount++
			lastErr = r.err
			continue
		}
		if retryableStatus[r.resp.StatusCode] {
			retryCount++
			r.resp.Body.Close()
			continue
		}
		// 拿到可用响应:胜出,取消其余路
		closeOnce.Do(func() { close(ctx) })
		// drain 剩余(其余路要么自己发结果进 resCh,要么因 ctx 关闭而关 resp)
		go func() {
			for k := collected; k < parallel; k++ {
				extra := <-resCh
				if extra.resp != nil {
					extra.resp.Body.Close()
				}
			}
		}()
		return r.resp, false, nil
	}

	// 全部回收完,无胜出者
	if netCount == parallel {
		return nil, false, lastErr
	}
	// retryCount == parallel(或部分重试+部分网络错,归为可重试)
	return nil, true, lastErr
}

// raceOne 发一路请求并替换鉴权头。返回 raceResult(含耗时)。
// idx 是本路编号(0-based),仅用于日志区分。
func (c *Client) raceOne(method, url string, body io.Reader, headers http.Header, idx int) raceResult {
	t0 := time.Now()
	resp, err := c.doOnce(method, url, body, headers)
	return raceResult{resp: resp, err: err, dur: time.Since(t0), idx: idx}
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
