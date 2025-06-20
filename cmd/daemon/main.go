package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sync4loong/internal/daemon"
	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "/etc/sync4loong/config.toml", "path to config file")
	flag.Parse()

	config, err := config.LoadFromFile(configPath)
	if err != nil {
		logger.Fatal("failed to load config", map[string]any{
			"config_path": configPath,
			"error":       err.Error(),
		})
	}

	daemon, err := daemon.NewDaemonService(config)
	if err != nil {
		logger.Fatal("failed to create daemon", map[string]any{
			"error": err.Error(),
		})
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		logger.Info("starting sync4loong daemon", nil)
		if err := daemon.Start(); err != nil {
			logger.Error("daemon start failed", err, nil)
		}
	}()

	sig := <-sigChan
	logger.Info("received shutdown signal", map[string]any{
		"signal": sig,
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := daemon.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", err, nil)
		os.Exit(1)
	}

	logger.Info("daemon stopped successfully", nil)
}
