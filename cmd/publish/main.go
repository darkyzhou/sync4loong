package main

import (
	"flag"

	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/publisher"
)

func main() {
	var (
		configPath = flag.String("config", "/etc/sync4loong/config.toml", "path to config file")
		folderPath = flag.String("folder", "", "path to folder to sync")
		prefix     = flag.String("prefix", "", "S3 prefix for uploaded files")
	)
	flag.Parse()

	if *folderPath == "" {
		logger.Fatal("folder path is required", nil)
	}
	if *prefix == "" {
		logger.Fatal("prefix is required", nil)
	}

	config, err := config.LoadFromFile(*configPath)
	if err != nil {
		logger.Fatal("failed to load config", map[string]any{
			"config_path": *configPath,
			"error":       err.Error(),
		})
	}

	publisher, err := publisher.NewPublisher(config)
	if err != nil {
		logger.Fatal("failed to create publisher", map[string]any{
			"error": err.Error(),
		})
	}
	defer publisher.Close()

	if err := publisher.PublishFileSyncTask(*folderPath, *prefix); err != nil {
		logger.Fatal("failed to publish task", map[string]any{
			"error": err.Error(),
		})
	}

	logger.Info("task published successfully", map[string]any{
		"folder_path": *folderPath,
		"prefix":      *prefix,
	})
}
