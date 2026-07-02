// Package config 加载代理运行配置(.env)。
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config 代理运行配置。
type Config struct {
	// ProxyHost 代理监听地址
	ProxyHost string
	// ProxyPort 代理监听端口
	ProxyPort int

	// ProxyKey 代理自身鉴权 key,Claude Code 用它当 ANTHROPIC_AUTH_TOKEN
	ProxyKey string

	// UpstreamBaseURL 科大上游 Anthropic 端点基址
	UpstreamBaseURL string
	// UpstreamAPIKey 科大上游 key(appid:secret 格式)
	UpstreamAPIKey string

	// GoogleSearchProxy 谷歌搜索代理(http://host:port),谷歌直连会超时
	GoogleSearchProxy string
	// GoogleSearchTimeout 谷歌搜索超时秒
	GoogleSearchTimeout int
	// GoogleSearchLimit 默认返回结果数
	GoogleSearchLimit int
}

// Load 从环境变量加载配置。缺失必要项返回 error。
func Load() (*Config, error) {
	cfg := &Config{
		ProxyHost:       getenv("PROXY_HOST", "0.0.0.0"),
		ProxyKey:        os.Getenv("KDX_PROXY_KEY"),
		UpstreamBaseURL: getenv("UPSTREAM_BASE_URL", "https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic"),
		UpstreamAPIKey:  os.Getenv("UPSTREAM_API_KEY"),

		GoogleSearchProxy:   os.Getenv("GOOGLE_SEARCH_PROXY"),
		GoogleSearchTimeout: getenvInt("GOOGLE_SEARCH_TIMEOUT", 15),
		GoogleSearchLimit:   getenvInt("GOOGLE_SEARCH_LIMIT", 5),
	}

	port, err := strconv.Atoi(getenv("PROXY_PORT", "8080"))
	if err != nil {
		return nil, fmt.Errorf("config: invalid PROXY_PORT: %w", err)
	}
	cfg.ProxyPort = port

	if cfg.ProxyKey == "" {
		return nil, fmt.Errorf("config: KDX_PROXY_KEY is required")
	}
	if cfg.UpstreamAPIKey == "" {
		return nil, fmt.Errorf("config: UPSTREAM_API_KEY is required")
	}
	return cfg, nil
}

// getenv 读环境变量,缺失返回 def。
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getenvInt 读环境变量为 int,缺失或非法返回 def。
func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
