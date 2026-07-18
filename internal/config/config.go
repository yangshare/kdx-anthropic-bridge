// Package config 加载代理运行配置(config.yaml)。
//
// 配置模型:多个上游平台(platforms),每个绑定一个 proxy_key 作为路由键;
// 改写规则与重试/并发参数由 profile 模板封装,平台引用 profile。
// 启动时完成全部校验与归一化,运行期不再处理配置错误。
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 代理运行配置。
type Config struct {
	Server       ServerConfig       `yaml:"server"`
	GoogleSearch GoogleSearchConfig `yaml:"google_search"`
	Platforms    []Platform         `yaml:"platforms"`
	Profiles     map[string]Profile `yaml:"profiles"`
}

// ServerConfig 网关自身监听配置。
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// GoogleSearchConfig 谷歌搜索(WebSearch 响应侧拦截用)。
type GoogleSearchConfig struct {
	Proxy   string `yaml:"proxy"`
	Timeout int    `yaml:"timeout"`
	Limit   int    `yaml:"limit"`
}

// Platform 上游实例。每个实例绑定一个全局唯一的 proxy_key 作为路由键。
type Platform struct {
	Name     string `yaml:"name"`
	ProxyKey string `yaml:"proxy_key"`
	BaseURL  string `yaml:"base_url"`
	APIKey   string `yaml:"api_key"`
	Profile  string `yaml:"profile"` // 引用 Profiles 的 key
}

// Profile 改写模板:封装该平台的协议适配开关 + 重试/并发/超时参数。
type Profile struct {
	RewriteThinking  bool     `yaml:"rewrite_thinking"`
	RewriteWebSearch bool     `yaml:"rewrite_web_search"`
	MaxRetries       int      `yaml:"max_retries"`
	RetryInterval    Duration `yaml:"retry_interval"`
	HeaderTimeout    Duration `yaml:"header_timeout"`
	Parallel         int      `yaml:"parallel"`
}

// Duration 包装 time.Duration,支持从 YAML 字符串解析("5s"/"30s"/"1m30s")。
type Duration time.Duration

// UnmarshalYAML 把 YAML 字符串解析为 time.Duration。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Load 从 path 读取并解析 config.yaml,执行校验与归一化。
// 任何配置错误都返回 error,由调用方 log.Fatal。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	// 严格模式:未知字段(拼写错误)报错,避免配置不生效而不察觉
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.normalize()
	return &cfg, nil
}

// validate 启动时校验,失败返回携带具体冲突项的 error。
func (c *Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range [1,65535]", c.Server.Port)
	}
	if len(c.Platforms) == 0 {
		return fmt.Errorf("config: platforms is empty (at least one upstream required)")
	}

	// proxy_key 全局唯一 + 必填 + base_url/api_key/profile 必填
	seen := make(map[string]bool, len(c.Platforms))
	for i, p := range c.Platforms {
		if p.ProxyKey == "" {
			return fmt.Errorf("config: platforms[%d].proxy_key is empty", i)
		}
		if seen[p.ProxyKey] {
			return fmt.Errorf("config: duplicate proxy_key %q", p.ProxyKey)
		}
		seen[p.ProxyKey] = true
		if p.BaseURL == "" {
			return fmt.Errorf("config: platforms[%d] (%s).base_url is empty", i, p.Name)
		}
		if p.APIKey == "" {
			return fmt.Errorf("config: platforms[%d] (%s).api_key is empty", i, p.Name)
		}
		if p.Profile == "" {
			return fmt.Errorf("config: platforms[%d] (%s).profile is empty", i, p.Name)
		}
		if _, ok := c.Profiles[p.Profile]; !ok {
			return fmt.Errorf("config: platforms[%d] (%s) references unknown profile %q",
				i, p.Name, p.Profile)
		}
	}
	return nil
}

// normalize 归一化缺省字段为安全默认值。
func (c *Config) normalize() {
	if c.GoogleSearch.Timeout <= 0 {
		c.GoogleSearch.Timeout = 15
	}
	if c.GoogleSearch.Limit <= 0 {
		c.GoogleSearch.Limit = 5
	}
	for name, p := range c.Profiles {
		if p.Parallel <= 0 {
			p.Parallel = 1
		}
		if p.HeaderTimeout <= 0 {
			p.HeaderTimeout = Duration(30 * time.Second)
		}
		c.Profiles[name] = p
	}
}

// Index 构造 proxy_key -> *Platform 反查表,供鉴权层 O(1) 查找。
func (c *Config) Index() map[string]*Platform {
	idx := make(map[string]*Platform, len(c.Platforms))
	for i := range c.Platforms {
		p := &c.Platforms[i]
		idx[p.ProxyKey] = p
	}
	return idx
}
