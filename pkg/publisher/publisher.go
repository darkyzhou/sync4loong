package publisher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

func (p *Publisher) PublishFileSyncTask(folderPath, prefix string) error {
	if folderPath == "" {
		return fmt.Errorf("folder path is required")
	}
	if prefix == "" {
		return fmt.Errorf("prefix is required")
	}

	if _, err := os.Stat(folderPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("folder not found: %s", folderPath)
	}

	entries, err := os.ReadDir(folderPath)
	if err != nil {
		return fmt.Errorf("read folder: %w", err)
	}

	hasValidFiles := false
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > 0 && name[0] != '.' {
			hasValidFiles = true
			break
		}
	}

	if !hasValidFiles {
		return fmt.Errorf("folder is empty or contains only hidden files: %s", folderPath)
	}

	payload := task.FileSyncPayload{
		FolderPath: folderPath,
		Prefix:     prefix,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	task := asynq.NewTask(task.TaskTypeFileSync, payloadBytes)
	info, err := p.client.Enqueue(
		task,
		asynq.MaxRetry(p.config.Publish.MaxRetry),
		asynq.Timeout(time.Duration(p.config.Publish.TimeoutMinutes)*time.Minute),
	)
	if err != nil {
		return fmt.Errorf("enqueue task: %w", err)
	}

	logger.Info("task enqueued successfully", map[string]any{
		"task_id": info.ID,
		"queue":   info.Queue,
		"folder":  folderPath,
		"prefix":  prefix,
	})
	return nil
}
