package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

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
}

func NewFileSyncHandler(s3Client *s3.S3, config *config.Config, redisClient *redis.Client, asyncClient *asynq.Client) *FileSyncHandler {
	sshDebouncer := ssh.NewDebouncer(redisClient, asyncClient, &config.Daemon, logger.NewDefault())

	return &FileSyncHandler{
		s3Client:     s3Client,
		config:       config,
		logger:       logger.NewDefault(),
		sshDebouncer: sshDebouncer,
	}
}

func (h *FileSyncHandler) Handle(ctx context.Context, asynqTask *asynq.Task) error {
	var payload task.FileSyncPayload
	if err := json.Unmarshal(asynqTask.Payload(), &payload); err != nil {
		h.logger.Error("failed to unmarshal payload", err, nil)
		return fmt.Errorf("unmarshal payload: %w", err)
	}

	h.logger.Info("starting file sync task", map[string]any{
		"folder_path": payload.FolderPath,
		"prefix":      payload.Prefix,
	})

	if err := h.syncFolder(ctx, &payload); err != nil {
		h.logger.Error("failed to sync folder", err, nil)
		return err
	}

	if err := h.sshDebouncer.TriggerSSH(ctx); err != nil {
		h.logger.Error("failed to trigger SSH debouncer", err, map[string]any{
			"folder_path": payload.FolderPath,
			"prefix":      payload.Prefix,
		})
	}

	return nil
}

func (h *FileSyncHandler) syncFolder(ctx context.Context, payload *task.FileSyncPayload) error {
	files, err := h.listFiles(payload.FolderPath)
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}

	return h.uploadFiles(ctx, files, payload)
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

func (h *FileSyncHandler) uploadFiles(ctx context.Context, files []string, payload *task.FileSyncPayload) error {
	for _, filePath := range files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		relPath, _ := filepath.Rel(payload.FolderPath, filePath)
		s3Key := strings.Join([]string{payload.Prefix, strings.ReplaceAll(relPath, "\\", "/")}, "/")

		shouldUpload, err := h.shouldUploadFile(ctx, filePath, s3Key)
		if err != nil {
			h.logger.Error("failed to check file status", err, map[string]any{
				"file_path": filePath,
				"s3_key":    s3Key,
			})
			continue
		}

		if !shouldUpload {
			h.logger.Info("file already exists with same size, skipping", map[string]any{
				"file_path": filePath,
				"s3_key":    s3Key,
			})
			continue
		}

		if err := h.uploadFile(ctx, filePath, s3Key); err != nil {
			h.logger.Error("failed to upload file", err, map[string]any{
				"file_path": filePath,
				"s3_key":    s3Key,
			})
			continue
		}

		h.logger.Info("uploaded file successfully", map[string]any{
			"file_path": filePath,
			"s3_key":    s3Key,
		})

		if h.config.Daemon.DeleteAfterSync {
			if err := h.deleteFile(filePath); err != nil {
				h.logger.Error("failed to delete file after sync", err, map[string]any{
					"file_path": filePath,
					"s3_key":    s3Key,
				})
			} else {
				h.logger.Info("deleted file after successful sync", map[string]any{
					"file_path": filePath,
					"s3_key":    s3Key,
				})
			}
		}
	}

	return nil
}

func (h *FileSyncHandler) shouldUploadFile(ctx context.Context, localPath, s3Key string) (bool, error) {
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

	contentType := h.getContentType(filePath)

	_, err = h.s3Client.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(h.config.S3.Bucket),
		Key:         aws.String(s3Key),
		ContentType: aws.String(contentType),
		Body:        file,
	})

	if err == nil {
		h.logger.Info("uploaded file with content type", map[string]any{
			"file_path":    filePath,
			"s3_key":       s3Key,
			"content_type": contentType,
		})
	}

	return err
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
			h.logger.Info("removed empty directory", map[string]any{
				"dir_path": dirPath,
			})
			h.cleanupEmptyDirectories(filepath.Dir(dirPath))
		}
	}
}
