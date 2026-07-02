// Package main 是 kdx-anthropic-bridge 代理入口。
//
// 启动后监听 PROXY_HOST:PROXY_PORT,接收 Claude Code 的 Anthropic 协议请求,
// 改写 thinking 字段后转发到科大上游,响应流式透传。
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/godkey/kdx-anthropic-bridge/internal/config"
	"github.com/godkey/kdx-anthropic-bridge/internal/server"
	"github.com/joho/godotenv"
)

func main() {
	// 启动时尝试加载同目录或可执行文件旁的 .env(找不到不报错,Docker/手动设环境变量时不用 .env)
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	srv := server.New(cfg)
	addr := fmt.Sprintf("%s:%d", cfg.ProxyHost, cfg.ProxyPort)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Routes(),
	}

	// 优雅退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		httpSrv.Close()
	}()

	log.Printf("kdx-anthropic-bridge listening on %s", addr)
	log.Printf("upstream: %s", cfg.UpstreamBaseURL)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
