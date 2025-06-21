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
	"sync4loong/pkg/s3"
	"sync4loong/pkg/task"
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

	s3Client, err := s3.CreateS3Client(config)
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}

	fileCache := cache.NewFileExistenceCache(redisClient, s3Client, config)

	fileSyncHandler := handler.NewFileSyncHandler(s3Client, config, redisClient, asyncClient, fileCache)
	sshHandler := handler.NewSSHHandler(&config.Daemon, logger.NewDefault(), redisClient, asyncClient)

	httpHandler, err := httpHandler.NewHTTPHandler(config, fileCache)
	if err != nil {
		return nil, fmt.Errorf("create http handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/publish", httpHandler.PublishHandler)
	mux.HandleFunc("/check/", httpHandler.CheckFileHandler)
	mux.HandleFunc("/check", httpHandler.CheckFileHandler)

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
		if err := d.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", err, nil)
		}
	}()

	logger.Info("starting Asynq server", nil)
	mux := asynq.NewServeMux()
	mux.HandleFunc(task.TaskTypeFileSync, d.fileSyncHandler.Handle)
	mux.HandleFunc(task.TaskTypeSSHCommand, d.sshHandler.ProcessTask)
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
