package daemon

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"sync4loong/pkg/cache"
	"sync4loong/pkg/config"
	"sync4loong/pkg/handler"
	httpHandler "sync4loong/pkg/http"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/shared"
	"sync4loong/pkg/storage"
)

type DaemonService struct {
	server          *asynq.Server
	httpServer      *http.Server
	fileSyncHandler *handler.FileSyncHandler
	sshHandler      *handler.SSHHandler
	httpHandler     *httpHandler.HTTPHandler
	config          *config.Config
}

func NewDaemonService(config *config.Config) (*DaemonService, error) {
	redisOpt := asynq.RedisClientOpt{
		Addr:     config.Redis.Addr,
		Password: config.Redis.Password,
		DB:       config.Redis.DB,
	}

	server := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: config.Daemon.Concurrency,
		Queues: map[string]int{
			"default": 6,
		},
	})

	redisClient := redis.NewClient(&redis.Options{
		Addr:     config.Redis.Addr,
		Password: config.Redis.Password,
		DB:       config.Redis.DB,
	})

	asyncClient := asynq.NewClient(redisOpt)

	logger.Info("creating storage backend", map[string]any{"type": config.Storage.Type})
	storageFactory := storage.NewStorageFactory()

	var storageBackend storage.StorageBackend
	var err error

	switch config.Storage.Type {
	case "s3":
		if config.Storage.S3 == nil {
			return nil, fmt.Errorf("S3 configuration is required when storage.type is 's3'")
		}
		storageBackend, err = storageFactory.CreateS3Backend(&storage.S3Config{
			Endpoint:                    config.Storage.S3.Endpoint,
			Region:                      config.Storage.S3.Region,
			Bucket:                      config.Storage.S3.Bucket,
			AccessKey:                   config.Storage.S3.AccessKey,
			SecretKey:                   config.Storage.S3.SecretKey,
			MaxRetries:                  config.Storage.S3.MaxRetries,
			FileUploadRetryCount:        config.Storage.S3.FileUploadRetryCount,
			FileUploadRetryDelaySeconds: config.Storage.S3.FileUploadRetryDelaySeconds,
			FileUploadTimeoutSeconds:    config.Storage.S3.FileUploadTimeoutSeconds,
			EnableIntegrityCheck:        config.Storage.S3.EnableIntegrityCheck,
		})
	case "sftp":
		if config.Storage.SFTP == nil {
			return nil, fmt.Errorf("SFTP configuration is required when storage.type is 'sftp'")
		}
		storageBackend, err = storageFactory.CreateSFTPBackend(&storage.SFTPConfig{
			Host:              config.Storage.SFTP.Host,
			Port:              config.Storage.SFTP.Port,
			Username:          config.Storage.SFTP.Username,
			Password:          config.Storage.SFTP.Password,
			PrivateKey:        config.Storage.SFTP.PrivateKey,
			ConnectionTimeout: config.Storage.SFTP.ConnectionTimeout,
			EnableResume:      config.Storage.SFTP.EnableResume,
		})
	default:
		return nil, fmt.Errorf("unsupported storage type: %s", config.Storage.Type)
	}

	if err != nil {
		return nil, fmt.Errorf("create storage backend: %w", err)
	}

	// Create cache for all storage backends using the storage backend interface
	fileCache := cache.NewFileExistenceCache(redisClient, storageBackend, config)

	fileSyncHandler := handler.NewFileSyncHandler(storageBackend, config, redisClient, asyncClient, fileCache)
	sshHandler := handler.NewSSHHandler(&config.Daemon, logger.NewDefault(), redisClient, asyncClient)

	httpHandler, err := httpHandler.NewHTTPHandler(config, fileCache)
	if err != nil {
		return nil, fmt.Errorf("create http handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/publish", httpHandler.PublishHandler)
	mux.HandleFunc("/check/", httpHandler.CheckFileHandler)
	mux.HandleFunc("/check", httpHandler.CheckFileHandler)
	mux.HandleFunc("/clear-cache/", httpHandler.ClearCacheHandler)
	mux.HandleFunc("/clear-cache", httpHandler.ClearCacheHandler)

	if asynqmonHandler := httpHandler.GetAsynqmonHandler(); asynqmonHandler != nil {
		mux.Handle(asynqmonHandler.RootPath()+"/", asynqmonHandler)
	}

	httpServer := &http.Server{
		Addr:    config.HTTP.Addr,
		Handler: mux,
	}

	return &DaemonService{
		server:          server,
		httpServer:      httpServer,
		fileSyncHandler: fileSyncHandler,
		sshHandler:      sshHandler,
		httpHandler:     httpHandler,
		config:          config,
	}, nil
}

func (d *DaemonService) Start() error {
	go func() {
		logger.Info("starting HTTP server", map[string]any{
			"addr": d.config.HTTP.Addr,
		})

		if d.config.Asynqmon.Enabled {
			logger.Info("asynqmon web UI enabled", map[string]any{
				"root_path":   d.config.Asynqmon.RootPath,
				"read_only":   d.config.Asynqmon.ReadOnlyMode,
				"prometheus":  d.config.Asynqmon.PrometheusAddr != "",
				"monitor_url": fmt.Sprintf("http://%s%s", d.config.HTTP.Addr, d.config.Asynqmon.RootPath),
			})
		}

		if err := d.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", err, nil)
		}
	}()

	logger.Info("starting Asynq server", nil)
	mux := asynq.NewServeMux()
	mux.HandleFunc(shared.TaskTypeFileSyncSingle, d.fileSyncHandler.HandleSingleFile)
	mux.HandleFunc(shared.TaskTypeSSHCommand, d.sshHandler.ProcessTask)
	return d.server.Run(mux)
}

func (d *DaemonService) Shutdown(ctx context.Context) error {
	logger.Info("initiating graceful shutdown", nil)

	if d.httpHandler != nil {
		d.httpHandler.Close()
	}

	if err := d.httpServer.Shutdown(ctx); err != nil {
		logger.Error("HTTP server shutdown failed", err, nil)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.server.Shutdown()
	}()

	select {
	case <-done:
		logger.Info("all tasks completed, shutdown successful", nil)
		return nil
	case <-ctx.Done():
		logger.Warn("shutdown timeout, forcing exit", nil)
		return ctx.Err()
	}
}
