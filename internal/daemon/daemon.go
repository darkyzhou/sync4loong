package daemon

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"sync4loong/pkg/config"
	"sync4loong/pkg/handler"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/s3"
	"sync4loong/pkg/task"
)

type DaemonService struct {
	server          *asynq.Server
	fileSyncHandler *handler.FileSyncHandler
	sshHandler      *handler.SSHHandler
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

	fileSyncHandler := handler.NewFileSyncHandler(s3Client, config, redisClient, asyncClient)
	sshHandler := handler.NewSSHHandler(&config.Daemon, logger.NewDefault(), redisClient, asyncClient)

	return &DaemonService{
		server:          server,
		fileSyncHandler: fileSyncHandler,
		sshHandler:      sshHandler,
		config:          config,
	}, nil
}

func (d *DaemonService) Start() error {
	mux := asynq.NewServeMux()
	mux.HandleFunc(task.TaskTypeFileSync, d.fileSyncHandler.Handle)
	mux.HandleFunc(task.TaskTypeSSHCommand, d.sshHandler.ProcessTask)
	return d.server.Run(mux)
}

func (d *DaemonService) Shutdown(ctx context.Context) error {
	logger.Info("initiating graceful shutdown", nil)

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
