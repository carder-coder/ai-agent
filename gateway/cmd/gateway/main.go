// 本文件负责读取网关配置、启动 HTTP 服务并处理系统终止信号。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/api"
	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

// main 启动网关，并在收到终止信号时尝试优雅关闭。
func main() {
	// 先完成配置与路由装配，再启动监听，避免服务处于半初始化状态。
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败：%v", err)
	}
	handler, err := api.NewRouter(cfg)
	if err != nil {
		log.Fatalf("初始化路由失败：%v", err)
	}

	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 监听放到 goroutine 中，主协程同时等待启动错误或终止信号。
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("gateway listening on %s", server.Addr)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("网关启动失败：%v", err)
		}
	case <-ctx.Done():
		// 先给进行中的请求最多 10 秒完成机会，超时后再强制关闭连接。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("优雅关停失败：%v", err)
			if closeErr := server.Close(); closeErr != nil {
				log.Printf("强制关闭失败：%v", closeErr)
			}
		}
		if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("网关关闭异常：%v", err)
		}
	}
}
