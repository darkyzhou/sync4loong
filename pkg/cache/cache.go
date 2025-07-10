package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/semaphore"

	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
)

type FileInfo struct {
	Exists    bool      `json:"exists"`
	Size      int64     `json:"size,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type CheckResult struct {
	Exists     bool      `json:"exists"`
	Cached     bool      `json:"cached"`
	Size       int64     `json:"size,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	TargetPath string    `json:"target_path"`
}

type FileExistenceCache struct {
	redisClient *redis.Client
	s3Client    *s3.S3
	config      *config.Config
	logger      *logger.Logger
	locks       sync.Map
	s3Semaphore *semaphore.Weighted
}

func NewFileExistenceCache(redisClient *redis.Client, s3Client *s3.S3, config *config.Config) *FileExistenceCache {
	return &FileExistenceCache{
		redisClient: redisClient,
		s3Client:    s3Client,
		config:      config,
		logger:      logger.NewDefault(),
		s3Semaphore: semaphore.NewWeighted(int64(config.Cache.MaxConcurrentS3Checks)),
	}
}

func (c *FileExistenceCache) CheckFileExists(ctx context.Context, targetPath string) (*CheckResult, error) {
	if err := c.validateTargetPath(targetPath); err != nil {
		return nil, err
	}

	cacheKey := c.getCacheKey(targetPath)
	cachedInfo, err := c.getCachedInfo(ctx, cacheKey)
	if err == nil {
		return &CheckResult{
			Exists:     cachedInfo.Exists,
			Cached:     true,
			Size:       cachedInfo.Size,
			Timestamp:  cachedInfo.Timestamp,
			TargetPath: targetPath,
		}, nil
	}

	lockKey := fmt.Sprintf("lock:%s", targetPath)
	if _, loaded := c.locks.LoadOrStore(lockKey, struct{}{}); loaded {
		for {
			time.Sleep(10 * time.Millisecond)
			if _, exists := c.locks.Load(lockKey); !exists {
				break
			}
		}

		cachedInfo, err := c.getCachedInfo(ctx, cacheKey)
		if err == nil {
			return &CheckResult{
				Exists:     cachedInfo.Exists,
				Cached:     true,
				Size:       cachedInfo.Size,
				Timestamp:  cachedInfo.Timestamp,
				TargetPath: targetPath,
			}, nil
		}
	}

	defer c.locks.Delete(lockKey)

	if err := c.s3Semaphore.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.s3Semaphore.Release(1)

	exists, size, err := c.checkS3FileExists(ctx, targetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to check S3: %w", err)
	}

	fileInfo := &FileInfo{
		Exists:    exists,
		Size:      size,
		Timestamp: time.Now(),
	}

	if c.redisClient != nil {
		if err := c.setCachedInfo(ctx, cacheKey, fileInfo); err != nil {
			c.logger.Error("failed to cache file info", err, map[string]any{
				"target_path": targetPath,
			})
		}
	}

	return &CheckResult{
		Exists:     exists,
		Cached:     false,
		Size:       size,
		Timestamp:  fileInfo.Timestamp,
		TargetPath: targetPath,
	}, nil
}

func (c *FileExistenceCache) SetFileExists(ctx context.Context, targetPath string, size int64) error {
	if err := c.validateTargetPath(targetPath); err != nil {
		return err
	}

	if c.redisClient == nil {
		return nil
	}

	cacheKey := c.getCacheKey(targetPath)
	fileInfo := &FileInfo{
		Exists:    true,
		Size:      size,
		Timestamp: time.Now(),
	}

	return c.setCachedInfo(ctx, cacheKey, fileInfo)
}

func (c *FileExistenceCache) GetAllowedPrefixes() []string {
	return c.config.Cache.AllowedPrefixes
}

func (c *FileExistenceCache) validateTargetPath(targetPath string) error {
	if targetPath == "" {
		return fmt.Errorf("target path cannot be empty")
	}

	if len(targetPath) > 1024 {
		return fmt.Errorf("target path too long")
	}

	cleanKey := filepath.Clean(targetPath)
	if strings.Contains(cleanKey, "..") {
		return fmt.Errorf("invalid target path: path traversal detected")
	}

	allowed := false
	for _, prefix := range c.config.Cache.AllowedPrefixes {
		if strings.HasPrefix(targetPath, prefix) {
			allowed = true
			break
		}
	}

	if !allowed {
		return fmt.Errorf("key not allowed: must start with allowed prefix")
	}

	return nil
}

func (c *FileExistenceCache) getCacheKey(targetPath string) string {
	return fmt.Sprintf("file_exists:%s:%s", c.config.Storage.S3.Bucket, targetPath)
}

func (c *FileExistenceCache) getCachedInfo(ctx context.Context, cacheKey string) (*FileInfo, error) {
	if c.redisClient == nil {
		return nil, fmt.Errorf("redis not available")
	}

	result, err := c.redisClient.Get(ctx, cacheKey).Result()
	if err != nil {
		return nil, err
	}

	var fileInfo FileInfo
	if err := json.Unmarshal([]byte(result), &fileInfo); err != nil {
		return nil, err
	}

	return &fileInfo, nil
}

func (c *FileExistenceCache) setCachedInfo(ctx context.Context, cacheKey string, fileInfo *FileInfo) error {
	data, err := json.Marshal(fileInfo)
	if err != nil {
		return err
	}

	return c.redisClient.Set(ctx, cacheKey, data, 0).Err()
}

func (c *FileExistenceCache) ClearCache(ctx context.Context, targetPath string) error {
	if targetPath != "" {
		if err := c.validateTargetPath(targetPath); err != nil {
			return err
		}

		if c.redisClient == nil {
			return nil
		}

		cacheKey := c.getCacheKey(targetPath)
		return c.redisClient.Del(ctx, cacheKey).Err()
	}

	if c.redisClient == nil {
		return nil
	}

	pattern := fmt.Sprintf("file_exists:%s:*", c.config.Storage.S3.Bucket)
	keys, err := c.redisClient.Keys(ctx, pattern).Result()
	if err != nil {
		return err
	}

	if len(keys) > 0 {
		return c.redisClient.Del(ctx, keys...).Err()
	}

	return nil
}

func (c *FileExistenceCache) checkS3FileExists(ctx context.Context, targetPath string) (bool, int64, error) {
	input := &s3.HeadObjectInput{
		Bucket: aws.String(c.config.Storage.S3.Bucket),
		Key:    aws.String(targetPath),
	}

	result, err := c.s3Client.HeadObjectWithContext(ctx, input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "NoSuchKey", "NotFound":
				return false, 0, nil
			default:
				return false, 0, err
			}
		}
		return false, 0, err
	}

	size := int64(0)
	if result.ContentLength != nil {
		size = *result.ContentLength
	}

	return true, size, nil
}
