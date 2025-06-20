# Sync4Loong Architecture Design

## Project Overview

Sync4Loong is a file synchronization system based on Go and Asynq, specifically designed for synchronizing local folders to S3 storage. The system consists of two core components:

- **publish**: Task publishing CLI tool, responsible for pushing file synchronization tasks to the queue
- **daemon**: Background daemon process, responsible for consuming the queue and executing file upload tasks

## Overall Architecture

```
┌─────────────┐    ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
│   publish   │───▶│   Redis     │◀───│   daemon    │───▶│ S3 Storage  │
│   (Client)  │    │  (Message   │    │  (Worker)   │    │ nix4loong   │
│             │    │   Broker)   │    │             │    │   bucket    │
└─────────────┘    └─────────────┘    └─────────────┘    └─────────────┘
       │                                     │
       │                                     │
       │           ┌─────────────┐           │
       └──────────▶│ Local Files │◀──────────┘
                   │   Folder    │
                   └─────────────┘
```

## Technology Stack

- **Task Queue**: Asynq (based on Redis)
- **Configuration Management**: Viper + TOML, supports environment variables, default values, validation and hot reload
- **Storage**: Supports S3-compatible object storage
- **Logging**: Structured logging (logfmt format), thread-safe
- **Validation**: go-playground/validator for configuration validation

## Core Data Structures

### Task Definition (pkg/task)

```go
const TaskTypeFileSync = "file_sync"

type FileSyncPayload struct {
    FolderPath string `json:"folder_path"`
    Prefix     string `json:"prefix"`
}

type SyncResult struct {
    FolderPath    string   `json:"folder_path"`
    Prefix        string   `json:"prefix"`
    TotalFiles    int      `json:"total_files"`
    UploadedFiles int      `json:"uploaded_files"`
    FailedFiles   []string `json:"failed_files"`
    Duration      string   `json:"duration"`
}
```

### Configuration Structure (pkg/config)

```go
type Config struct {
    Redis   RedisConfig   `toml:"redis" validate:"required"`
    S3      S3Config      `toml:"s3" validate:"required"`
    Daemon  DaemonConfig  `toml:"daemon" validate:"required"`
    Publish PublishConfig `toml:"publish" validate:"required"`
}

type RedisConfig struct {
    Addr     string `toml:"addr" validate:"required,hostname_port"`
    Password string `toml:"password"`
    DB       int    `toml:"db" validate:"min=0,max=15"`
}

type S3Config struct {
    Endpoint  string `toml:"endpoint" validate:"required,url"`
    Region    string `toml:"region" validate:"required,min=1"`
    Bucket    string `toml:"bucket" validate:"required,min=1"`
    AccessKey string `toml:"access_key" validate:"required,min=1"`
    SecretKey string `toml:"secret_key" validate:"required,min=1"`
}

type DaemonConfig struct {
    Concurrency        int    `toml:"concurrency" validate:"required,min=1,max=100"`
    LogLevel           string `toml:"log_level" validate:"required,oneof=debug info warn error fatal"`
    SSHCommand         string `toml:"ssh_command"`
    SSHDebounceMinutes int    `toml:"ssh_debounce_minutes" validate:"min=1"`
    SSHTimeoutMinutes  int    `toml:"ssh_timeout_minutes" validate:"min=1"`
    EnableSSHTask      bool   `toml:"enable_ssh_task"`
    DeleteAfterSync    bool   `toml:"delete_after_sync"`
}

type PublishConfig struct {
    MaxRetry       int `toml:"max_retry" validate:"required,min=0,max=10"`
    TimeoutMinutes int `toml:"timeout_minutes" validate:"required,min=1,max=1440"`
}
```

## Core Component Design

### 1. File Sync Handler (pkg/handler/FileSyncHandler)

- Responsible for handling file synchronization tasks
- Supports intelligent file skipping (based on file size comparison)
- Cross-platform S3 path handling (Windows backslash conversion)
- Context cancellation support
- Optional file deletion after successful sync (configurable via `delete_after_sync`)
- Automatic cleanup of empty directories after file deletion

### 2. Task Publisher (pkg/publisher/Publisher)

- Validates local folder existence and non-emptiness
- Creates and pushes tasks to Redis queue
- Supports retry and timeout configuration

### 3. Daemon Service (internal/daemon/DaemonService)

- Asynq server management
- Graceful shutdown support

### 4. Logging System (pkg/logger/Logger)

- Thread-safe structured logging
- logfmt format output
- Supports different log levels

## Configuration Management

### Viper Configuration Features

- **Multi-source support**: Configuration files, environment variables, default values
- **Environment variables**: Supports `SYNC4LOONG_` prefix environment variable overrides
- **Hot reload**: Automatically reloads when configuration files change
- **Type safety**: Automatically parses to structs and validates
- **Default values**: Automatically sets reasonable defaults:
  - Redis address: localhost:6379
  - Log level: info
  - Concurrency: 4
  - Max retry: 3
  - Timeout: 30 minutes

## Directory Structure

```
sync4loong/
├── cmd/
│   ├── publish/           # Task publishing CLI
│   └── daemon/            # Daemon process
├── pkg/
│   ├── config/            # Configuration management (with validation)
│   ├── task/              # Task definitions
│   ├── handler/           # File sync handler
│   ├── publisher/         # Task publisher
│   ├── logger/            # Thread-safe logging
│   └── s3/                # S3 client wrapper
├── internal/
│   └── daemon/            # Daemon service
├── Makefile               # Build scripts
├── go.mod                 # Go module definition
└── README.md              # Project documentation
```

## Core Workflow

### 1. Task Publishing Flow

1. CLI validates command line arguments (folder path, S3 prefix)
2. Checks local folder existence and non-emptiness
3. Creates `FileSyncPayload` task payload
4. Pushes task to Redis queue
5. Returns task ID and status

### 2. Task Execution Flow

1. Daemon retrieves tasks from Redis queue
2. Recursively traverses all files in the specified folder
3. Generates S3 key for each file: `prefix + relative path`
4. Executes intelligent upload decision:
   - File doesn't exist → Upload
   - File size differs → Overwrite upload
   - File size same → Skip
5. Records detailed upload logs and results
6. Optionally deletes source files after successful upload (if `delete_after_sync` is enabled)
7. Automatically cleans up empty directories after file deletion

### 3. File Path Mapping

```
Local: /data/nix-store/bin/bash
S3:    store/bin/bash

Local: /data/nix-store/lib/libc.so.6
S3:    store/lib/libc.so.6
```
