package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/task"
)

const (
	sshDebounceStateKey = "ssh_debounce_state"
)

type DebounceState struct {
	LastRequestTime   int64 `json:"last_request_time"`
	PendingTaskExists bool  `json:"pending_task_exists"`
}

type Debouncer struct {
	redisClient *redis.Client
	asyncClient *asynq.Client
	config      *config.DaemonConfig
	logger      *logger.Logger
}

func NewDebouncer(redisClient *redis.Client, asyncClient *asynq.Client, config *config.DaemonConfig, logger *logger.Logger) *Debouncer {
	return &Debouncer{
		redisClient: redisClient,
		asyncClient: asyncClient,
		config:      config,
		logger:      logger,
	}
}

func (d *Debouncer) TriggerSSH(ctx context.Context) error {
	if !d.config.EnableSSHTask || d.config.SSHCommand == "" {
		return nil
	}

	now := time.Now().Unix()

	state, err := d.getDebounceState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get debounce state: %w", err)
	}

	state.LastRequestTime = now

	if !state.PendingTaskExists {
		state.PendingTaskExists = true

		if err := d.saveDebounceState(ctx, state); err != nil {
			return fmt.Errorf("failed to save debounce state: %w", err)
		}

		delay := time.Duration(d.config.SSHDebounceMinutes) * time.Minute
		payload := task.SSHPayload{
			Command: d.config.SSHCommand,
		}

		taskPayload, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("failed to marshal SSH payload: %w", err)
		}

		sshTask := asynq.NewTask(task.TaskTypeSSHCommand, taskPayload)
		if _, err := d.asyncClient.Enqueue(sshTask, asynq.ProcessIn(delay)); err != nil {
			return fmt.Errorf("failed to enqueue SSH task: %w", err)
		}

		d.logger.Info("SSH debounce task created", map[string]any{
			"delay_minutes": d.config.SSHDebounceMinutes,
		})
	} else {
		if err := d.saveDebounceState(ctx, state); err != nil {
			return fmt.Errorf("failed to save debounce state: %w", err)
		}

		d.logger.Info("SSH debounce request updated", map[string]any{
			"pending_task_exists": true,
		})
	}

	return nil
}

func (d *Debouncer) getDebounceState(ctx context.Context) (*DebounceState, error) {
	result, err := d.redisClient.Get(ctx, sshDebounceStateKey).Result()
	if err != nil {
		if err == redis.Nil {
			return &DebounceState{
				LastRequestTime:   0,
				PendingTaskExists: false,
			}, nil
		}
		return nil, err
	}

	var state DebounceState
	if err := json.Unmarshal([]byte(result), &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal debounce state: %w", err)
	}

	return &state, nil
}

func (d *Debouncer) saveDebounceState(ctx context.Context, state *DebounceState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal debounce state: %w", err)
	}

	expiration := time.Duration(d.config.SSHDebounceMinutes*2) * time.Minute
	return d.redisClient.Set(ctx, sshDebounceStateKey, data, expiration).Err()
}

func (d *Debouncer) MarkTaskCompleted(ctx context.Context) error {
	state, err := d.getDebounceState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get debounce state: %w", err)
	}

	state.PendingTaskExists = false
	if err := d.saveDebounceState(ctx, state); err != nil {
		return fmt.Errorf("failed to save debounce state: %w", err)
	}
	return nil
}

func (d *Debouncer) ShouldExecuteSSH(ctx context.Context) (bool, error) {
	state, err := d.getDebounceState(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get debounce state: %w", err)
	}

	now := time.Now().Unix()
	debounceWindow := int64(d.config.SSHDebounceMinutes * 60)
	if now-state.LastRequestTime < debounceWindow {
		return false, nil
	}
	return true, nil
}
