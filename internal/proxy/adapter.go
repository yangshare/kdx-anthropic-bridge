package proxy

import (
	"context"
	"time"

	"github.com/godkey/kdx-anthropic-bridge/internal/config"
	"github.com/godkey/kdx-anthropic-bridge/internal/search"
)

// WebSearchExecutorAdapter 把 search.GoogleSearcher 包装成 WebSearchExecutor。
type WebSearchExecutorAdapter struct {
	google *search.GoogleSearcher
}

// NewSearchAdapter 从配置构造搜索执行器。
func NewSearchAdapter(cfg *config.Config) *WebSearchExecutorAdapter {
	timeout := time.Duration(cfg.GoogleSearchTimeout) * time.Second
	return &WebSearchExecutorAdapter{
		google: &search.GoogleSearcher{
			Proxy:   cfg.GoogleSearchProxy,
			Timeout: timeout,
		},
	}
}

// Search 实现 WebSearchExecutor 接口。
func (a *WebSearchExecutorAdapter) Search(ctx context.Context, query string, limit int) ([]search.Item, error) {
	return a.google.Search(ctx, query, limit)
}
