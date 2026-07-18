package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTempYAML 把 content 写入临时文件，返回路径。t.Cleanup 自动清理。
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return p
}

// fullYAML 规格第 3.1 节完整示例（双平台 + 双 profile）。
const fullYAML = `server:
  host: 0.0.0.0
  port: 8080

google_search:
  proxy: http://127.0.0.1:7890
  timeout: 15
  limit: 5

platforms:
  - name: kdx
    proxy_key: token-kdx-xxxxxx
    base_url: https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic
    api_key: appid:secret
    profile: keding

  - name: anthropic
    proxy_key: token-anthropic-yyyyyy
    base_url: https://api.anthropic.com
    api_key: sk-ant-xyz
    profile: official

profiles:
  keding:
    rewrite_thinking: true
    rewrite_web_search: true
    max_retries: 10
    retry_interval: 5s
    header_timeout: 30s
    parallel: 1

  official:
    rewrite_thinking: false
    rewrite_web_search: false
    max_retries: 0
    header_timeout: 60s
    parallel: 1
`

func TestLoad_fullExample(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, fullYAML))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("server.host = %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("server.port = %d", cfg.Server.Port)
	}
	if cfg.GoogleSearch.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("google_search.proxy = %q", cfg.GoogleSearch.Proxy)
	}
	if cfg.GoogleSearch.Timeout != 15 {
		t.Errorf("google_search.timeout = %d", cfg.GoogleSearch.Timeout)
	}
	if cfg.GoogleSearch.Limit != 5 {
		t.Errorf("google_search.limit = %d", cfg.GoogleSearch.Limit)
	}

	if len(cfg.Platforms) != 2 {
		t.Fatalf("platforms count = %d, want 2", len(cfg.Platforms))
	}
	kdx := cfg.Platforms[0]
	if kdx.Name != "kdx" || kdx.ProxyKey != "token-kdx-xxxxxx" || kdx.Profile != "keding" {
		t.Errorf("kdx platform mismatch: %+v", kdx)
	}
	if kdx.BaseURL != "https://maas-coding-api.cn-huabei-1.xf-yun.com/anthropic" {
		t.Errorf("kdx base_url = %q", kdx.BaseURL)
	}
	if kdx.APIKey != "appid:secret" {
		t.Errorf("kdx api_key = %q", kdx.APIKey)
	}

	keding := cfg.Profiles["keding"]
	if !keding.RewriteThinking || !keding.RewriteWebSearch {
		t.Errorf("keding rewrite flags wrong: %+v", keding)
	}
	if keding.MaxRetries != 10 {
		t.Errorf("keding max_retries = %d", keding.MaxRetries)
	}
	// Duration 字符串解析
	if time.Duration(keding.RetryInterval) != 5*time.Second {
		t.Errorf("keding retry_interval = %v, want 5s", keding.RetryInterval)
	}
	if time.Duration(keding.HeaderTimeout) != 30*time.Second {
		t.Errorf("keding header_timeout = %v, want 30s", keding.HeaderTimeout)
	}
	if keding.Parallel != 1 {
		t.Errorf("keding parallel = %d", keding.Parallel)
	}
}

func TestLoad_durationParsed(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p:
    retry_interval: 1m30s
    header_timeout: 2s
`
	cfg, err := Load(writeTempYAML(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Profiles["p"]
	if time.Duration(p.RetryInterval) != 90*time.Second {
		t.Errorf("retry_interval = %v, want 90s", p.RetryInterval)
	}
	if time.Duration(p.HeaderTimeout) != 2*time.Second {
		t.Errorf("header_timeout = %v, want 2s", p.HeaderTimeout)
	}
}

func TestLoad_invalidDuration(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p: {header_timeout: not-a-duration}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("invalid duration should error")
	}
}

func TestLoad_fileNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("missing file should error")
	}
}

func TestLoad_emptyPlatforms(t *testing.T) {
	yaml := `server: {port: 8080}
platforms: []
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("empty platforms should error")
	}
}

func TestLoad_duplicateProxyKey(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: dup, base_url: http://x, api_key: ak, profile: p}
  - {name: b, proxy_key: dup, base_url: http://y, api_key: ak, profile: p}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("duplicate proxy_key should error")
	}
}

func TestLoad_unknownProfileRef(t *testing.T) {
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: ghost}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("unknown profile reference should error")
	}
}

func TestLoad_invalidPort(t *testing.T) {
	yaml := `server: {port: 99999}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("invalid port should error")
	}
}

func TestLoad_unknownFieldRejected(t *testing.T) {
	// KnownFields(true):拼错字段名应启动报错
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p, rewite_thinking: true}
profiles:
  p: {}
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("unknown field should error in strict mode")
	}
}

func TestLoad_normalizeDefaults(t *testing.T) {
	// parallel/header_timeout/google_search 缺省时归一化
	yaml := `server: {port: 8080}
platforms:
  - {name: a, proxy_key: k1, base_url: http://x, api_key: ak, profile: p}
profiles:
  p: {}
`
	cfg, err := Load(writeTempYAML(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Profiles["p"]
	if p.Parallel != 1 {
		t.Errorf("parallel default = %d, want 1", p.Parallel)
	}
	if time.Duration(p.HeaderTimeout) != 30*time.Second {
		t.Errorf("header_timeout default = %v, want 30s", p.HeaderTimeout)
	}
	if cfg.GoogleSearch.Timeout != 15 {
		t.Errorf("google_search.timeout default = %d, want 15", cfg.GoogleSearch.Timeout)
	}
	if cfg.GoogleSearch.Limit != 5 {
		t.Errorf("google_search.limit default = %d, want 5", cfg.GoogleSearch.Limit)
	}
}

func TestIndex_proxyKeyLookup(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, fullYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	idx := cfg.Index()
	if len(idx) != 2 {
		t.Fatalf("index size = %d, want 2", len(idx))
	}
	kdx, ok := idx["token-kdx-xxxxxx"]
	if !ok || kdx.Name != "kdx" {
		t.Errorf("kdx lookup failed: %+v", kdx)
	}
	ant, ok := idx["token-anthropic-yyyyyy"]
	if !ok || ant.Name != "anthropic" {
		t.Errorf("anthropic lookup failed: %+v", ant)
	}
	if _, ok := idx["unknown-key"]; ok {
		t.Error("unknown key should not resolve")
	}
}
