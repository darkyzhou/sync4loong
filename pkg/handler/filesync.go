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
	"sync"
	"time"

	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/semaphore"

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

type uploadTask struct {
	filePath        string
	s3Key           string
	deleteAfterSync bool
	overwrite       bool
}

type uploadResult struct {
	task     uploadTask
	err      error
	skipped  bool
	uploaded bool
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

func (h *FileSyncHandler) Handle(ctx context.Context, asynqTask *asynq.Task) error {
	var payload task.FileSyncPayload
	if err := json.Unmarshal(asynqTask.Payload(), &payload); err != nil {
		h.logger.Error("failed to unmarshal payload", err, nil)
		return fmt.Errorf("unmarshal payload: %w", err)
	}

	h.logger.Info("starting file sync task", map[string]any{
		"items_count": len(payload.Items),
	})

	if err := h.syncItems(ctx, &payload); err != nil {
		h.logger.Error("failed to sync items", err, nil)
		return err
	}

	if err := h.sshDebouncer.TriggerSSH(ctx); err != nil {
		h.logger.Error("failed to trigger SSH debouncer", err, map[string]any{
			"items_count": len(payload.Items),
		})
	}

	return nil
}

func (h *FileSyncHandler) syncItems(ctx context.Context, payload *task.FileSyncPayload) error {
	for _, item := range payload.Items {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := h.syncSingleItem(ctx, &item); err != nil {
			h.logger.Error("failed to sync item", err, map[string]any{
				"from": item.From,
				"to":   item.To,
			})
			return fmt.Errorf("sync item %s -> %s: %w", item.From, item.To, err)
		}
	}
	return nil
}

func (h *FileSyncHandler) syncSingleItem(ctx context.Context, item *task.SyncItem) error {
	// Resolve symlink to get real path
	realPath, err := filepath.EvalSymlinks(item.From)
	if err != nil {
		return fmt.Errorf("resolve symlink %s: %w", item.From, err)
	}

	// Use the real path for all subsequent operations
	from := realPath

	// Check if source is a file or directory
	stat, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", from, err)
	}

	if stat.IsDir() {
		// Handle directory sync - treat 'To' as target directory
		files, err := h.listFiles(from)
		if err != nil {
			return fmt.Errorf("list files in %s: %w", from, err)
		}
		// Create a new item with the resolved path and preserve DeleteAfterSync and Overwrite settings
		resolvedItem := &task.SyncItem{From: from, To: item.To, DeleteAfterSync: item.DeleteAfterSync, Overwrite: item.Overwrite}
		return h.uploadFiles(ctx, files, resolvedItem)
	} else {
		// Handle single file sync
		// If 'To' ends with '/', treat it as a directory and append filename
		s3Key := item.To
		if strings.HasSuffix(item.To, "/") {
			filename := filepath.Base(from)
			s3Key = item.To + filename
		}
		return h.uploadSingleFileWithOptions(ctx, from, s3Key, item.DeleteAfterSync, item.Overwrite)
	}
}

func (h *FileSyncHandler) listFiles(folderPath string) ([]string, error) {
	var files []string

	err := filepath.Walk(folderPath, func(path string, info os.FileInfo, err error) error {
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

func (h *FileSyncHandler) uploadFiles(ctx context.Context, files []string, item *task.SyncItem) error {
	if len(files) == 0 {
		return nil
	}

	concurrency := h.config.S3.FileUploadConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(files) {
		concurrency = len(files)
	}

	return h.uploadFilesConcurrently(ctx, files, item, concurrency)
}

func (h *FileSyncHandler) uploadFilesConcurrently(ctx context.Context, files []string, item *task.SyncItem, concurrency int) error {
	resultsCh := make(chan uploadResult, len(files))

	var wg sync.WaitGroup
	sem := semaphore.NewWeighted(int64(concurrency))
	for _, filePath := range files {
		relPath, _ := filepath.Rel(item.From, filePath)
		s3Key := strings.Join([]string{item.To, strings.ReplaceAll(relPath, "\\", "/")}, "/")
		s3Key = strings.ReplaceAll(s3Key, "//", "/")

		task := uploadTask{
			filePath:        filePath,
			s3Key:           s3Key,
			deleteAfterSync: item.DeleteAfterSync,
			overwrite:       item.Overwrite,
		}

		wg.Add(1)
		go func(task uploadTask) {
			defer wg.Done()

			if err := sem.Acquire(ctx, 1); err != nil {
				resultsCh <- uploadResult{task: task, err: err}
				return
			}
			defer sem.Release(1)

			result := h.processUploadTask(ctx, task)
			resultsCh <- result
		}(task)
	}

	// Wait for all goroutines to complete and close results channel
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var errors []error
	totalFiles := len(files)
	uploadedCount := 0
	skippedCount := 0

	for result := range resultsCh {
		if result.err != nil {
			h.logger.Error("failed to upload file", result.err, map[string]any{
				"file_path": result.task.filePath,
				"s3_key":    result.task.s3Key,
			})
			errors = append(errors, result.err)
		} else if result.uploaded {
			uploadedCount++
		} else if result.skipped {
			skippedCount++
		}
	}

	h.logger.Info("concurrent upload completed", map[string]any{
		"total_files":    totalFiles,
		"uploaded_files": uploadedCount,
		"skipped_files":  skippedCount,
		"failed_files":   len(errors),
		"from":           item.From,
		"to":             item.To,
	})

	if len(errors) > 0 {
		return fmt.Errorf("failed to upload %d out of %d files", len(errors), totalFiles)
	}
	return nil
}

func (h *FileSyncHandler) processUploadTask(ctx context.Context, task uploadTask) uploadResult {
	shouldUpload, err := h.shouldUploadFile(ctx, task.filePath, task.s3Key, task.overwrite)
	if err != nil {
		return uploadResult{task: task, err: fmt.Errorf("check file status: %w", err)}
	}

	if !shouldUpload {
		h.logger.Info("file already exists with same size, skipping", map[string]any{
			"file_path": task.filePath,
			"s3_key":    task.s3Key,
		})
		if task.deleteAfterSync {
			if err := h.deleteFile(task.filePath); err != nil {
				h.logger.Error("failed to delete file after sync", err, map[string]any{
					"file_path": task.filePath,
					"s3_key":    task.s3Key,
				})
			}
		}
		return uploadResult{task: task, skipped: true}
	}

	if err := h.uploadFile(ctx, task.filePath, task.s3Key); err != nil {
		return uploadResult{task: task, err: fmt.Errorf("upload file: %w", err)}
	}

	if task.deleteAfterSync {
		if err := h.deleteFile(task.filePath); err != nil {
			h.logger.Error("failed to delete file after sync", err, map[string]any{
				"file_path": task.filePath,
				"s3_key":    task.s3Key,
			})
		}
	}
	return uploadResult{task: task, uploaded: true}
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
