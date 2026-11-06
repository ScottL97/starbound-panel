// Command starbound-panel runs the Starbound server web panel.
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

	"github.com/ScottL97/starbound-panel/internal/config"
	"github.com/ScottL97/starbound-panel/internal/server"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg := config.FromEnv(version)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	srv, err := server.New(cfg, logger)
	if err != nil {
		logger.Error("cannot start panel", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		logger.Error("http server failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	// Stop the game server first so it can save the universe, then drop the
	// (long-lived) SSE connections.
	srv.Shutdown(shutdownCtx)
	_ = httpSrv.Close()
}
