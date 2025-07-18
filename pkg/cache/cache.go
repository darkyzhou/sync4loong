package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/semaphore"

	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/storage"
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
	redisClient      *redis.Client
	storageBackend   storage.StorageBackend
	config           *config.Config
	logger           *logger.Logger
	locks            sync.Map
	storageSemaphore *semaphore.Weighted
}

func NewFileExistenceCache(redisClient *redis.Client, storageBackend storage.StorageBackend, config *config.Config) *FileExistenceCache {
	return &FileExistenceCache{
		redisClient:      redisClient,
		storageBackend:   storageBackend,
		config:           config,
		logger:           logger.NewDefault(),
		storageSemaphore: semaphore.NewWeighted(int64(config.Cache.MaxConcurrentStorageChecks)),
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

	if err := c.storageSemaphore.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.storageSemaphore.Release(1)

	metadata, err := c.storageBackend.CheckFileExists(ctx, targetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to check storage backend: %w", err)
	}

	exists := metadata.Exists
	size := metadata.Size

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
	return fmt.Sprintf("file_exists:%s:%s", c.storageBackend.GetCacheIdentifier(), targetPath)
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

	pattern := fmt.Sprintf("file_exists:%s:*", c.storageBackend.GetCacheIdentifier())
	keys, err := c.redisClient.Keys(ctx, pattern).Result()
	if err != nil {
		return err
	}

	if len(keys) > 0 {
		return c.redisClient.Del(ctx, keys...).Err()
	}

	return nil
}
