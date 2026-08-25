// cumora-server —— Go 单二进制服务端骨架(#51 · ADR 0004)。
//
// 本票只立骨架:启动/配置/pg 连接/goose 迁移基线/healthz/日志/优雅停机。
// 域实现按 #52+ 逐票落地,验收以验收镜像(CUMORA_MIRROR_BASE 指向本进程)
// 双跑为准。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func main() {
	cfg := config.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	pool, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		slog.Error("pg connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(pool, cfg.MigrationsDir); err != nil {
		slog.Error("go_migrations apply failed", "err", err)
		os.Exit(1)
	}
	slog.Info("schema at baseline", "migrations", cfg.MigrationsDir)

	mux := http.NewServeMux()
	httpx.MountHealth(mux, pool)
	// 域路由按票挂载(#52 认证 → #53 会话 → …),全部经 httpx 助手以对齐
	// 契约(packages/contract)与镜像断言。

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown incomplete", "err", err)
	}
	slog.Info("bye")
}
