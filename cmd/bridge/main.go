// Package main 是 kdx-anthropic-bridge 代理入口。
//
// 启动后读取 config.yaml(--config 指定,默认工作目录下 config.yaml),
// 监听 server.host:server.port,接收 Claude Code 的 Anthropic 协议请求,
// 按 proxy key 路由到对应上游平台,响应流式透传。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/godkey/kdx-anthropic-bridge/internal/config"
	"github.com/godkey/kdx-anthropic-bridge/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	srv := server.New(cfg)
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
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
	for i := range cfg.Platforms {
		p := &cfg.Platforms[i]
		log.Printf("platform %s -> %s (profile=%s)", p.Name, p.BaseURL, p.Profile)
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
