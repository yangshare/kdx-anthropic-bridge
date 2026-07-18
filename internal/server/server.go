// Package server 实现代理 HTTP 服务。
//
// 接入层职责:接收 Claude Code 请求、按 proxy key 路由到对应上游平台、
// 调用业务层(proxy.RewriteRequest)按平台 profile 改写、
// 调用基础层(upstream.Client)转发、流式回传响应。不写业务判断。
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

// platformRuntime 平台运行态:配置 + 预解析 profile + 该平台专属上游客户端。
// 鉴权层 pickPlatform 返回 *platformRuntime,handleAll 直接取 client.Forward。
type platformRuntime struct {
	cfg     config.Platform
	profile config.Profile
	client  *upstream.Client
}

// Server 代理 HTTP 服务。
type Server struct {
	cfg        *config.Config
	byProxyKey map[string]*platformRuntime
	rewriter   func([]byte, proxy.RewriteOptions) (*proxy.RewriteResult, error)
	// searcher 谷歌搜索执行器,响应侧拦截 web_search tool_use 时用。
	// 为 nil 时不做响应过滤(web_search 走原样透传)。
	searcher    *proxy.WebSearchExecutorAdapter
	googleLimit int
}

// New 构造 Server:为每个平台构造独立 *upstream.Client(注入该平台 profile
// 的 HeaderTimeout 作 transport ResponseHeaderTimeout、MaxRetries、RetryInterval、
// Parallel,以及平台 BaseURL/APIKey),建立 proxy_key 反查表。
func New(cfg *config.Config) *Server {
	byProxyKey := make(map[string]*platformRuntime, len(cfg.Platforms))
	for i := range cfg.Platforms {
		pc := &cfg.Platforms[i]
		prof := cfg.Profiles[pc.Profile] // 校验已保证存在

		// 每平台独立 transport:ResponseHeaderTimeout 随 profile 变化,平台间互不干扰。
		// 用 ResponseHeaderTimeout 而非 http.Client.Timeout:
		// 只限"等上游响应头"的时间,一旦开始流式返回,传输不限总时长
		// (长文档/长思考可慢慢流,不会被掐断)。
		transport := &http.Transport{
			ResponseHeaderTimeout: time.Duration(prof.HeaderTimeout),
		}
		client := &upstream.Client{
			BaseURL:       pc.BaseURL,
			APIKey:        pc.APIKey,
			HTTP:          &http.Client{Transport: transport},
			MaxRetries:    prof.MaxRetries,
			RetryInterval: time.Duration(prof.RetryInterval),
			Parallel:      prof.Parallel,
		}
		byProxyKey[pc.ProxyKey] = &platformRuntime{
			cfg:     *pc,
			profile: prof,
			client:  client,
		}
	}

	s := &Server{
		cfg:         cfg,
		byProxyKey:  byProxyKey,
		rewriter:    proxy.RewriteRequest,
		googleLimit: cfg.GoogleSearch.Limit,
	}
	// 配了谷歌搜索代理才启用 web_search 响应过滤
	if cfg.GoogleSearch.Proxy != "" {
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

// handleAll 统一入口:按 proxy key 路由到平台,鉴权 + 改写 + 转发 + 流式回传。
func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	p := s.pickPlatform(r)
	if p == nil {
		writeError(w, http.StatusUnauthorized, "invalid or unknown proxy key")
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
		result, err := s.rewriter(body, proxy.RewriteOptions{
			Thinking:  p.profile.RewriteThinking,
			WebSearch: p.profile.RewriteWebSearch,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "rewrite request body failed")
			return
		}
		body = result.Body
		hasWebSearch = result.HasWebSearch
	}

	// 透传路径(含 query)。body 以 []byte 传入,支持上游重试时重放
	tStart := time.Now()
	resp, err := p.client.Forward(r.Method, r.URL.RequestURI(), body, r.Header)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream forward failed")
		log.Printf("upstream error platform=%s after %s: %v", p.cfg.Name, time.Since(tStart), err)
		return
	}
	defer resp.Body.Close()
	headerWait := time.Since(tStart)

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 仅当 profile 开了 rewrite_web_search、请求含 web_search、且配了搜索器,
	// 才走流式拦截;否则原样透传
	if hasWebSearch && p.profile.RewriteWebSearch && s.searcher != nil {
		filter := proxy.NewStreamFilter(s.searcher, s.googleLimit)
		if err := filter.FilterStream(r.Context(), w, resp.Body); err != nil {
			log.Printf("stream filter error: %v", err)
		}
		return
	}

	// 其他:流式透传
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		log.Printf("stream copy error platform=%s path=%s: %v", p.cfg.Name, r.URL.Path, err)
	}
	log.Printf("done platform=%s path=%s status=%d header_wait=%s stream=%s total=%s bytes=%d",
		p.cfg.Name, r.URL.Path, resp.StatusCode, headerWait, time.Since(tStart)-headerWait,
		time.Since(tStart), n)
}

// pickPlatform 从请求头提取 token,反查命中则返回该平台运行态;未命中返回 nil。
func (s *Server) pickPlatform(r *http.Request) *platformRuntime {
	token := extractToken(r)
	if token == "" {
		return nil
	}
	return s.byProxyKey[token]
}

// extractToken 从 Authorization: Bearer <t> 或 x-api-key: <t> 提取 token。
func extractToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			return auth[len(prefix):]
		}
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	return ""
}
