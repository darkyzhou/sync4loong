package handler

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"sync4loong/pkg/cache"
	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/ssh"
	"sync4loong/pkg/task"
)

type FileSyncHandler struct {
	s3Client     *s3.S3
	config       *config.Config
	logger       *logger.Logger
	sshDebouncer *ssh.Debouncer
	cache        *cache.FileExistenceCache
}

func NewFileSyncHandler(s3Client *s3.S3, config *config.Config, redisClient *redis.Client, asyncClient *asynq.Client, cache *cache.FileExistenceCache) *FileSyncHandler {
	sshDebouncer := ssh.NewDebouncer(redisClient, asyncClient, &config.Daemon, logger.NewDefault())

	return &FileSyncHandler{
		s3Client:     s3Client,
		config:       config,
		logger:       logger.NewDefault(),
		sshDebouncer: sshDebouncer,
		cache:        cache,
	}
}

// HandleSingleFile handles single file sync task
func (h *FileSyncHandler) HandleSingleFile(ctx context.Context, asynqTask *asynq.Task) error {
	var payload task.FileSyncSinglePayload
	if err := json.Unmarshal(asynqTask.Payload(), &payload); err != nil {
		h.logger.Error("failed to unmarshal single file payload", err, nil)
		return fmt.Errorf("unmarshal single file payload: %w", asynq.SkipRetry)
	}

	// Process single file
	if err := h.uploadSingleFileWithOptions(ctx, payload.FilePath, payload.S3Key, payload.DeleteAfterSync, payload.Overwrite); err != nil {
		h.logger.Error("failed to upload single file", err, map[string]any{
			"file_path": payload.FilePath,
			"s3_key":    payload.S3Key,
		})
		return fmt.Errorf("upload single file %s -> %s: %w", payload.FilePath, payload.S3Key, err)
	}

	h.logger.Info("single file sync completed", map[string]any{
		"file_path": payload.FilePath,
		"s3_key":    payload.S3Key,
	})

	// Trigger SSH debouncer after successful file sync
	if err := h.sshDebouncer.TriggerSSH(ctx); err != nil {
		h.logger.Error("failed to trigger SSH debouncer", err, map[string]any{
			"file_path": payload.FilePath,
			"s3_key":    payload.S3Key,
		})
		// Don't fail the task if SSH trigger fails
	}

	return nil
}

func (h *FileSyncHandler) uploadSingleFileWithOptions(ctx context.Context, filePath, s3Key string, deleteAfterSync bool, overwrite bool) error {
	shouldUpload, err := h.shouldUploadFile(ctx, filePath, s3Key, overwrite)
	if err != nil {
		return fmt.Errorf("check file status: %w", err)
	}

	if !shouldUpload {
		h.logger.Info("file already exists with same size, skipping", map[string]any{
			"file_path": filePath,
			"s3_key":    s3Key,
		})
	} else {
		if err := h.uploadFile(ctx, filePath, s3Key); err != nil {
			return fmt.Errorf("upload file: %w", err)
		}
	}

	if deleteAfterSync {
		if err := h.deleteFile(filePath); err != nil {
			h.logger.Error("failed to delete file after sync", err, map[string]any{
				"file_path": filePath,
				"s3_key":    s3Key,
			})
		}
	}

	return nil
}

func (h *FileSyncHandler) shouldUploadFile(ctx context.Context, localPath, s3Key string, overwrite bool) (bool, error) {
	if overwrite {
		return true, nil
	}

	localStat, err := os.Stat(localPath)
	if err != nil {
		return false, fmt.Errorf("stat local file: %w", err)
	}
	localSize := localStat.Size()

	headResp, err := h.s3Client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.config.S3.Bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "NoSuchKey", "NotFound":
				return true, nil
			default:
				return false, fmt.Errorf("head object: %w", err)
			}
		}
		return false, fmt.Errorf("head object: %w", err)
	}

	s3Size := *headResp.ContentLength
	if localSize != s3Size {
		h.logger.Info("file size differs, will overwrite", map[string]any{
			"s3_key":     s3Key,
			"local_size": localSize,
			"s3_size":    s3Size,
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

func (h *FileSyncHandler) uploadFile(ctx context.Context, filePath, s3Key string) error {
	var lastErr error
	retryCount := h.config.S3.FileUploadRetryCount
	retryDelay := time.Duration(h.config.S3.FileUploadRetryDelaySeconds) * time.Second

	for attempt := 0; attempt <= retryCount; attempt++ {
		if attempt > 0 {
			h.logger.Info("retrying file upload", map[string]any{
				"file_path":    filePath,
				"s3_key":       s3Key,
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

		err := h.uploadFileOnce(ctx, filePath, s3Key)
		if err == nil {
			h.logger.Info("uploaded file successfully", map[string]any{
				"file_path": filePath,
				"s3_key":    s3Key,
				"attempts":  attempt + 1,
			})
			return nil
		}

		lastErr = err

		if !h.isRetryableError(err) {
			h.logger.Error("non-retryable error, giving up", err, map[string]any{
				"file_path": filePath,
				"s3_key":    s3Key,
				"attempt":   attempt + 1,
			})
			return err
		}

		h.logger.Error("retryable error occurred", err, map[string]any{
			"file_path": filePath,
			"s3_key":    s3Key,
			"attempt":   attempt + 1,
		})
	}

	h.logger.Error("max retry attempts exceeded", lastErr, map[string]any{
		"file_path":    filePath,
		"s3_key":       s3Key,
		"max_attempts": retryCount + 1,
	})
	return fmt.Errorf("upload failed after %d attempts: %w", retryCount+1, lastErr)
}

func (h *FileSyncHandler) uploadFileOnce(ctx context.Context, filePath, s3Key string) error {
	timeout := time.Duration(h.config.S3.FileUploadTimeoutSeconds) * time.Second
	uploadCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	h.logger.Info("starting file upload with timeout", map[string]any{
		"file_path":       filePath,
		"s3_key":          s3Key,
		"timeout_seconds": h.config.S3.FileUploadTimeoutSeconds,
	})

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			h.logger.Error("failed to close file", err, map[string]any{
				"file_path": filePath,
			})
		}
	}()

	fileStat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	fileSize := fileStat.Size()

	contentType := h.getContentType(filePath)

	putObjectInput := &s3.PutObjectInput{
		Bucket:      aws.String(h.config.S3.Bucket),
		Key:         aws.String(s3Key),
		ContentType: aws.String(contentType),
		Body:        file,
	}

	if h.config.S3.EnableIntegrityCheck {
		if _, err := file.Seek(0, 0); err != nil {
			return fmt.Errorf("seek file: %w", err)
		}

		md5Hash, err := h.calculateMD5(file)
		if err != nil {
			return fmt.Errorf("calculate MD5: %w", err)
		}

		putObjectInput.ContentMD5 = aws.String(md5Hash)
		if _, err := file.Seek(0, 0); err != nil {
			return fmt.Errorf("seek file for upload: %w", err)
		}
	}

	_, err = h.s3Client.PutObjectWithContext(uploadCtx, putObjectInput)
	if err != nil {
		if uploadCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("upload timed out after %d seconds: %w", h.config.S3.FileUploadTimeoutSeconds, err)
		}
		return fmt.Errorf("put object: %w", err)
	}

	if h.cache != nil {
		if err := h.cache.SetFileExists(ctx, s3Key, fileSize); err != nil {
			h.logger.Error("failed to update cache after upload", err, map[string]any{
				"s3_key": s3Key,
			})
		}
	}

	return nil
}

func (h *FileSyncHandler) calculateMD5(file *os.File) (string, error) {
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(hash.Sum(nil)), nil
}

func (h *FileSyncHandler) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}

	var aerr awserr.Error
	if errors.As(err, &aerr) {
		switch aerr.Code() {
		case "RequestTimeout", "ServiceUnavailable", "Throttling", "ThrottlingException",
			"ProvisionedThroughputExceededException", "RequestTimeTooSkewed":
			return true
		case "InvalidAccessKeyId", "SignatureDoesNotMatch", "NoSuchBucket", "AccessDenied":
			return false
		}
	}

	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "temporary failure") ||
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "connection timed out") {
		return true
	}

	return false
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
