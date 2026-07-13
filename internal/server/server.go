// Package server 实现代理 HTTP 服务。
//
// 接入层职责:接收 Claude Code 请求、鉴权(KDX_PROXY_KEY)、
// 调用业务层(proxy.RewriteRequest)改写、调用基础层(upstream.Client)转发、
// 流式回传响应。不写业务判断。
package server

import (
	"io"
	"log"
	"net/http"
	"time"

	"github.com/godkey/kdx-anthropic-bridge/internal/anthropic"
	"github.com/godkey/kdx-anthropic-bridge/internal/config"
	"github.com/godkey/kdx-anthropic-bridge/internal/proxy"
	"github.com/godkey/kdx-anthropic-bridge/internal/upstream"
)

// Server 代理 HTTP 服务。
type Server struct {
	cfg      *config.Config
	rewriter func([]byte) (*proxy.RewriteResult, error)
	upstream *upstream.Client
	// searcher 谷歌搜索执行器,响应侧拦截 web_search tool_use 时用。
	// 为 nil 时不做响应过滤(web_search 走原样透传)。
	searcher *proxy.WebSearchExecutorAdapter
}

// New 构造 Server。
func New(cfg *config.Config) *Server {
	// 用 ResponseHeaderTimeout 而非 http.Client.Timeout:
	// 只限"等上游响应头"的时间,一旦开始流式返回,传输不限总时长
	// (长文档/长思考可慢慢流,不会被掐断)。
	transport := &http.Transport{
		ResponseHeaderTimeout: cfg.UpstreamHeaderTimeout,
	}
	s := &Server{
		cfg:      cfg,
		rewriter: proxy.RewriteRequest,
		upstream: &upstream.Client{
			BaseURL:       cfg.UpstreamBaseURL,
			APIKey:        cfg.UpstreamAPIKey,
			HTTP:          &http.Client{Transport: transport},
			MaxRetries:    cfg.UpstreamMaxRetries,
			RetryInterval: cfg.UpstreamRetryInterval,
			Parallel:      cfg.UpstreamParallel,
		},
	}
	// 配了谷歌搜索代理才启用 web_search 响应过滤
	if cfg.GoogleSearchProxy != "" {
		s.searcher = proxy.NewSearchAdapter(cfg)
	}
	return s
}

// Routes 返回配置好路由的 http.Handler。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleAll)
	return mux
}

// handleAll 统一入口:所有路径都走鉴权 + 透传逻辑。
// /v1/messages 会做 thinking 改写,其他路径原样透传。
func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid proxy key")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body failed")
		return
	}
	hasWebSearch := false

	// 仅 /v1/messages 改写请求体,其他路径原样透传
	if r.URL.Path == anthropic.PathMessages && r.Method == http.MethodPost {
		result, err := s.rewriter(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "rewrite request body failed")
			return
		}
		body = result.Body
		hasWebSearch = result.HasWebSearch
	}

	// 透传路径(含 query)。body 以 []byte 传入,支持上游重试时重放
	tStart := time.Now()
	resp, err := s.upstream.Forward(r.Method, r.URL.RequestURI(), body, r.Header)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream forward failed")
		log.Printf("upstream error after %s: %v", time.Since(tStart), err)
		return
	}
	defer resp.Body.Close()
	headerWait := time.Since(tStart)

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 含 web_search 且配了搜索器:用流式过滤器拦截 web_search tool_use
	if hasWebSearch && s.searcher != nil {
		filter := proxy.NewStreamFilter(s.searcher, s.cfg.GoogleSearchLimit)
		if err := filter.FilterStream(r.Context(), w, resp.Body); err != nil {
			log.Printf("stream filter error: %v", err)
		}
		return
	}

	// 其他:流式透传
	n, _ := io.Copy(w, resp.Body)
	log.Printf("done path=%s status=%d header_wait=%s stream=%s total=%s bytes=%d",
		r.URL.Path, resp.StatusCode, headerWait, time.Since(tStart)-headerWait,
		time.Since(tStart), n)
}

// authorized 校验 Claude Code 侧的鉴权头是否等于 KDX_PROXY_KEY。
func (s *Server) authorized(r *http.Request) bool {
	// Authorization: Bearer <key>
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			return auth[len(prefix):] == s.cfg.ProxyKey
		}
	}
	// x-api-key: <key>
	if key := r.Header.Get("x-api-key"); key != "" {
		return key == s.cfg.ProxyKey
	}
	return false
}
