package server

import (
	"io"
	"net/http"
)

// copyHeaders 逐项复制上游响应头到下游响应头(不复制 hop-by-hop 头)。
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		// 跳过 hop-by-hop 头,这些由 transport 管理
		if isHopHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// isHopHeader 判断是否为 hop-by-hop 头。
func isHopHeader(h string) bool {
	switch http.CanonicalHeaderKey(h) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailers",
		"Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// writeError 写一个简单的 JSON 错误响应。
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, `{"error":{"type":"proxy_error","message":"`+msg+`"}}`)
}
