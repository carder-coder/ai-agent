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

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败：%v", err)
	}

	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           api.NewRouter(cfg),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
