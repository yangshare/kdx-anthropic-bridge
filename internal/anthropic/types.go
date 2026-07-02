// Package anthropic 定义 Anthropic 协议相关常量与辅助。
//
// 本包只放协议层常量,不定义完整请求/响应结构体——
// 代理用 map[string]any 透传未知字段,避免丢字段(见 internal/proxy/rewriter.go)。
package anthropic

// 路径常量
const (
	// PathMessages /v1/messages 主链入口
	PathMessages = "/v1/messages"
	// PathCountTokens /v1/messages/count_tokens
	PathCountTokens = "/v1/messages/count_tokens"
)

// 鉴权头
const (
	HeaderAuthorization = "Authorization"
	HeaderXAPIKey       = "x-api-key"
	HeaderAnthropicVer  = "anthropic-version"
)
