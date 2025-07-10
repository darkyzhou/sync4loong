package publisher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/task"

	"github.com/hibiken/asynq"
)

type Publisher struct {
	client *asynq.Client
	config *config.Config
}

func NewPublisher(config *config.Config) (*Publisher, error) {
	redisOpt := asynq.RedisClientOpt{
		Addr:     config.Redis.Addr,
		Password: config.Redis.Password,
		DB:       config.Redis.DB,
	}

	client := asynq.NewClient(redisOpt)

	return &Publisher{
		client: client,
		config: config,
	}, nil
}

func (p *Publisher) Close() {
	_ = p.client.Close()
}

type SyncItem struct {
	From            string `json:"from"`
	To              string `json:"to"`
	DeleteAfterSync bool   `json:"delete_after_sync,omitempty"`
	Overwrite       bool   `json:"overwrite,omitempty"`
}

// scanFiles recursively scans a directory and returns all file paths
func (p *Publisher) scanFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// generateTargetPath generates target path from target path and file path
func (p *Publisher) generateTargetPath(targetPath, filePath, basePath string) string {
	// Get relative path from base path
	relPath, err := filepath.Rel(basePath, filePath)
	if err != nil {
		// If can't get relative path, use filename
		relPath = filepath.Base(filePath)
	}

	// Convert Windows path separators to forward slashes
	relPath = strings.ReplaceAll(relPath, "\\", "/")

	// Combine with target path
	if strings.HasSuffix(targetPath, "/") {
		return targetPath + relPath
	}
	return targetPath + "/" + relPath
}

// PublishFileSyncTaskAsFiles publishes file sync task as individual file tasks
func (p *Publisher) PublishFileSyncTaskAsFiles(items []SyncItem) error {
	if len(items) == 0 {
		return fmt.Errorf("at least one sync item is required")
	}

	var totalFiles int

	for i, item := range items {
		if item.From == "" {
			return fmt.Errorf("'from' field is required for item %d", i)
		}
		if item.To == "" {
			return fmt.Errorf("'to' field is required for item %d", i)
		}

		// Resolve symlink to get real path
		realPath, err := filepath.EvalSymlinks(item.From)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", item.From, err)
		}

		stat, err := os.Stat(realPath)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("source not found: %s", realPath)
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", realPath, err)
		}

		var files []string
		var basePath string
		if stat.IsDir() {
			// For directory, scan all files
			files, err = p.scanFiles(realPath)
			if err != nil {
				return fmt.Errorf("scan directory %s: %w", realPath, err)
			}
			if len(files) == 0 {
				return fmt.Errorf("directory is empty: %s", realPath)
			}
			basePath = realPath
		} else {
			// For single file
			files = []string{realPath}
			basePath = realPath
		}

		// Create task for each file
		for _, file := range files {
			var targetPath string
			if stat.IsDir() {
				targetPath = p.generateTargetPath(item.To, file, basePath)
			} else {
				// For single file, use target path as is
				targetPath = item.To
			}

			payload := task.FileSyncSinglePayload{
				FilePath:        file,
				TargetPath:      targetPath,
				DeleteAfterSync: item.DeleteAfterSync,
				Overwrite:       item.Overwrite,
			}

			payloadBytes, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("marshal payload for %s: %w", file, err)
			}

			taskObj := asynq.NewTask(task.TaskTypeFileSyncSingle, payloadBytes)
			_, err = p.client.Enqueue(
				taskObj,
				asynq.MaxRetry(p.config.Publish.MaxRetry),
				asynq.Timeout(time.Duration(p.config.Publish.TimeoutMinutes)*time.Minute),
				asynq.Retention(24*time.Hour),
			)
			if err != nil {
				return fmt.Errorf("enqueue task for %s: %w", file, err)
			}
			totalFiles++
		}
	}

	logger.Info("file sync tasks enqueued successfully", map[string]any{
		"total_files": totalFiles,
		"items_count": len(items),
	})
	return nil
}
