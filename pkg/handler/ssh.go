package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/ssh"
	"sync4loong/pkg/task"
)

type SSHHandler struct {
	config      *config.DaemonConfig
	logger      *logger.Logger
	debouncer   *ssh.Debouncer
	asyncClient *asynq.Client
}

func NewSSHHandler(config *config.DaemonConfig, logger *logger.Logger, redisClient *redis.Client, asyncClient *asynq.Client) *SSHHandler {
	debouncer := ssh.NewDebouncer(redisClient, asyncClient, config, logger)

	return &SSHHandler{
		config:      config,
		logger:      logger,
		debouncer:   debouncer,
		asyncClient: asyncClient,
	}
}

func (h *SSHHandler) ProcessTask(ctx context.Context, t *asynq.Task) error {
	var payload task.SSHPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("failed to unmarshal SSH payload: %w", err)
	}

	shouldExecute, err := h.debouncer.ShouldExecuteSSH(ctx)
	if err != nil {
		return fmt.Errorf("failed to check SSH execution condition: %w", err)
	}
	if !shouldExecute {
		delay := time.Duration(h.config.SSHDebounceMinutes) * time.Minute
		newTask := asynq.NewTask(task.TaskTypeSSHCommand, t.Payload())

		if _, err := h.asyncClient.Enqueue(newTask, asynq.ProcessIn(delay)); err != nil {
			h.logger.Error("Failed to reschedule SSH task", err, map[string]any{
				"delay_minutes": h.config.SSHDebounceMinutes,
			})
			return fmt.Errorf("failed to reschedule SSH task: %w", err)
		}

		h.logger.Info("SSH task rescheduled due to debounce", map[string]any{
			"delay_minutes": h.config.SSHDebounceMinutes,
		})
		return nil
	}

	startTime := time.Now()

	h.logger.Info("Executing SSH command", map[string]any{
		"command":         payload.Command,
		"timeout_minutes": h.config.SSHTimeoutMinutes,
	})

	parts := strings.Fields(payload.Command)
	if len(parts) == 0 {
		return fmt.Errorf("empty SSH command")
	}

	timeout := time.Duration(h.config.SSHTimeoutMinutes) * time.Minute
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, parts[0], parts[1:]...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)
	if err != nil {
		var errorType string
		if timeoutCtx.Err() == context.DeadlineExceeded {
			errorType = "timeout"
		} else {
			errorType = "command_error"
		}

		h.logger.Error("ssh command failed", err, map[string]any{
			"command":         payload.Command,
			"output":          string(output),
			"duration":        duration,
			"timeout_minutes": h.config.SSHTimeoutMinutes,
			"error_type":      errorType,
		})

		if markErr := h.debouncer.MarkTaskCompleted(ctx); markErr != nil {
			h.logger.Error("failed to mark ssh task as completed", markErr, nil)
		}

		return fmt.Errorf("ssh command execution failed: %w", err)
	}

	h.logger.Info("ssh command executed successfully", map[string]any{
		"command":         payload.Command,
		"output":          string(output),
		"duration":        duration,
		"timeout_minutes": h.config.SSHTimeoutMinutes,
	})

	if err := h.debouncer.MarkTaskCompleted(ctx); err != nil {
		h.logger.Error("failed to mark ssh task as completed", err, nil)
		return fmt.Errorf("failed to mark SSH task as completed: %w", err)
	}

	return nil
}
