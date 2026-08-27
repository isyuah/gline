package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/isyuah/gline/internal/server/bootstrap"
	"github.com/isyuah/gline/internal/server/config"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.FromEnv()
	if err != nil {
		logger.Error("invalid server configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := bootstrap.New(ctx, cfg, version, logger)
	if err != nil {
		logger.Error("initialize server", "error", err)
		os.Exit(1)
	}
	if err := application.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("server stopped with error", "error", err)
		os.Exit(1)
	}
}
