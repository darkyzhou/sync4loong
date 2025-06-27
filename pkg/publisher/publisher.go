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

type SyncItem struct {
	From            string `json:"from"`
	To              string `json:"to"`
	DeleteAfterSync bool   `json:"delete_after_sync,omitempty"`
}

func (p *Publisher) PublishFileSyncTask(items []SyncItem) error {
	if len(items) == 0 {
		return fmt.Errorf("at least one sync item is required")
	}

	// Validate all items first
	for i, item := range items {
		if item.From == "" {
			return fmt.Errorf("'from' field is required for item %d", i)
		}
		if item.To == "" {
			return fmt.Errorf("'to' field is required for item %d", i)
		}

		// Check if source path exists
		if _, err := os.Stat(item.From); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("source not found: %s", item.From)
		}

		// If it's a directory, check if it has valid files
		if stat, err := os.Stat(item.From); err == nil && stat.IsDir() {
			entries, err := os.ReadDir(item.From)
			if err != nil {
				return fmt.Errorf("read directory %s: %w", item.From, err)
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
				return fmt.Errorf("directory is empty or contains only hidden files: %s", item.From)
			}
		}
	}

	// Convert to new payload format
	syncItems := make([]task.SyncItem, len(items))
	for i, item := range items {
		syncItems[i] = task.SyncItem{
			From:            item.From,
			To:              item.To,
			DeleteAfterSync: item.DeleteAfterSync,
		}
	}

	payload := task.FileSyncPayload{
		Items: syncItems,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	taskObj := asynq.NewTask(task.TaskTypeFileSync, payloadBytes)
	info, err := p.client.Enqueue(
		taskObj,
		asynq.MaxRetry(p.config.Publish.MaxRetry),
		asynq.Timeout(time.Duration(p.config.Publish.TimeoutMinutes)*time.Minute),
	)
	if err != nil {
		return fmt.Errorf("enqueue task: %w", err)
	}

	logger.Info("task enqueued successfully", map[string]any{
		"task_id":     info.ID,
		"queue":       info.Queue,
		"items_count": len(items),
	})
	return nil
}
