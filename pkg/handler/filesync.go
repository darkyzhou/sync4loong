package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"sync4loong/pkg/cache"
	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/ssh"
	"sync4loong/pkg/storage"
	"sync4loong/pkg/task"
)

type FileSyncHandler struct {
	storageBackend storage.StorageBackend
	config         *config.Config
	logger         *logger.Logger
	sshDebouncer   *ssh.Debouncer
	cache          *cache.FileExistenceCache
}

func NewFileSyncHandler(storageBackend storage.StorageBackend, config *config.Config, redisClient *redis.Client, asyncClient *asynq.Client, cache *cache.FileExistenceCache) *FileSyncHandler {
	sshDebouncer := ssh.NewDebouncer(redisClient, asyncClient, &config.Daemon, logger.NewDefault())

	return &FileSyncHandler{
		storageBackend: storageBackend,
		config:         config,
		logger:         logger.NewDefault(),
		sshDebouncer:   sshDebouncer,
		cache:          cache,
	}
}

// HandleSingleFile handles single file sync task
func (h *FileSyncHandler) HandleSingleFile(ctx context.Context, asynqTask *asynq.Task) error {
	var payload task.FileSyncSinglePayload
	if err := json.Unmarshal(asynqTask.Payload(), &payload); err != nil {
		h.logger.Error("failed to unmarshal single file payload", err, nil)
		return fmt.Errorf("unmarshal single file payload: %w", asynq.SkipRetry)
	}

	if err := h.uploadSingleFileWithOptions(ctx, payload.FilePath, payload.TargetPath, payload.DeleteAfterSync, payload.Overwrite); err != nil {
		h.logger.Error("failed to upload single file", err, map[string]any{
			"file_path":   payload.FilePath,
			"target_path": payload.TargetPath,
		})
		return fmt.Errorf("upload single file %s -> %s: %w", payload.FilePath, payload.TargetPath, err)
	}

	h.logger.Info("single file sync completed", map[string]any{
		"file_path":   payload.FilePath,
		"target_path": payload.TargetPath,
	})

	if err := h.sshDebouncer.TriggerSSH(ctx); err != nil {
		h.logger.Error("failed to trigger SSH debouncer", err, map[string]any{
			"file_path":   payload.FilePath,
			"target_path": payload.TargetPath,
		})
		// Don't fail the task if SSH trigger fails
	}

	return nil
}

func (h *FileSyncHandler) uploadSingleFileWithOptions(ctx context.Context, filePath, targetPath string, deleteAfterSync bool, overwrite bool) error {
	shouldUpload, err := h.shouldUploadFile(ctx, filePath, targetPath, overwrite)
	if err != nil {
		return fmt.Errorf("check file status: %w", err)
	}

	if !shouldUpload {
		h.logger.Info("file already exists with same size, skipping", map[string]any{
			"file_path":   filePath,
			"target_path": targetPath,
		})
	} else {
		if err := h.uploadFile(ctx, filePath, targetPath, overwrite); err != nil {
			return fmt.Errorf("upload file: %w", err)
		}
	}

	if deleteAfterSync {
		if err := h.deleteFile(filePath); err != nil {
			h.logger.Error("failed to delete file after sync", err, map[string]any{
				"file_path":   filePath,
				"target_path": targetPath,
			})
		}
	}

	return nil
}

func (h *FileSyncHandler) shouldUploadFile(ctx context.Context, localPath, targetPath string, overwrite bool) (bool, error) {
	if overwrite {
		return true, nil
	}

	localStat, err := os.Stat(localPath)
	if err != nil {
		return false, fmt.Errorf("stat local file: %w", err)
	}
	localSize := localStat.Size()

	metadata, err := h.storageBackend.CheckFileExists(ctx, targetPath)
	if err != nil {
		var storageErr *storage.StorageError
		if errors.As(err, &storageErr) && storageErr.Type == storage.ErrorTypeNotFound {
			return true, nil
		}
		return false, fmt.Errorf("check file existence: %w", err)
	}

	if !metadata.Exists {
		return true, nil
	}

	if localSize != metadata.Size {
		h.logger.Info("file size differs, will overwrite", map[string]any{
			"target_path": targetPath,
			"local_size":  localSize,
			"remote_size": metadata.Size,
		})
		return true, nil
	}

	return false, nil
}

func (h *FileSyncHandler) getContentType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".nar":
		return "application/x-nix-archive"
	case ".narinfo":
		return "text/x-nix-narinfo"
	}

	contentType := mime.TypeByExtension(ext)

	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return contentType
}

func (h *FileSyncHandler) uploadFile(ctx context.Context, filePath, targetPath string, overwrite bool) error {
	var lastErr error
	retryCount := h.config.Storage.S3.FileUploadRetryCount
	retryDelay := time.Duration(h.config.Storage.S3.FileUploadRetryDelaySeconds) * time.Second

	for attempt := 0; attempt <= retryCount; attempt++ {
		if attempt > 0 {
			h.logger.Info("retrying file upload", map[string]any{
				"file_path":    filePath,
				"target_path":  targetPath,
				"attempt":      attempt + 1,
				"max_attempts": retryCount + 1,
			})

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}

			retryDelay *= 2
		}

		err := h.uploadFileOnce(ctx, filePath, targetPath, overwrite)
		if err == nil {
			h.logger.Info("uploaded file successfully", map[string]any{
				"file_path":   filePath,
				"target_path": targetPath,
				"attempts":    attempt + 1,
			})
			return nil
		}

		lastErr = err

		if !storage.IsRetryableError(err) {
			h.logger.Error("non-retryable error, giving up", err, map[string]any{
				"file_path":   filePath,
				"target_path": targetPath,
				"attempt":     attempt + 1,
			})
			return err
		}

		h.logger.Error("retryable error occurred", err, map[string]any{
			"file_path":   filePath,
			"target_path": targetPath,
			"attempt":     attempt + 1,
		})
	}

	h.logger.Error("max retry attempts exceeded", lastErr, map[string]any{
		"file_path":    filePath,
		"target_path":  targetPath,
		"max_attempts": retryCount + 1,
	})
	return fmt.Errorf("upload failed after %d attempts: %w", retryCount+1, lastErr)
}

func (h *FileSyncHandler) uploadFileOnce(ctx context.Context, filePath, targetPath string, overwrite bool) error {
	h.logger.Info("starting file upload", map[string]any{
		"file_path":   filePath,
		"target_path": targetPath,
	})

	contentType := h.getContentType(filePath)
	opts := &storage.UploadOptions{
		Overwrite:   overwrite,
		ContentType: contentType,
	}

	if err := h.storageBackend.UploadFile(ctx, filePath, targetPath, opts); err != nil {
		return err
	}

	if h.cache != nil {
		fileStat, err := os.Stat(filePath)
		if err == nil {
			if err := h.cache.SetFileExists(ctx, targetPath, fileStat.Size()); err != nil {
				h.logger.Error("failed to update cache after upload", err, map[string]any{
					"target_path": targetPath,
				})
			}
		}
	}

	return nil
}

func (h *FileSyncHandler) deleteFile(filePath string) error {
	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("remove file: %w", err)
	}

	h.cleanupEmptyDirectories(filepath.Dir(filePath))
	return nil
}

func (h *FileSyncHandler) cleanupEmptyDirectories(dirPath string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}

	if len(entries) == 0 {
		if err := os.Remove(dirPath); err == nil {
			h.cleanupEmptyDirectories(filepath.Dir(dirPath))
		}
	}
}
