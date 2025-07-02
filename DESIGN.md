# Sync4Loong Architecture Design

## Project Overview

Sync4Loong is a file synchronization system based on Go and Asynq, specifically designed for synchronizing local folders to S3 storage. The system consists of one core component:

- **daemon**: Background daemon process with integrated HTTP API, responsible for accepting task submissions via HTTP and consuming the queue to execute file upload tasks

## Overall Architecture

```
┌─────────────┐    ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
│HTTP Client  │───▶│   daemon    │───▶│   Redis     │    │ S3 Storage  │
│(curl/wget)  │    │ (HTTP API + │    │  (Message   │    │ nix4loong   │
│             │    │  Worker)    │    │   Broker)   │    │   bucket    │
└─────────────┘    └─────────────┘    └─────────────┘    └─────────────┘
                           │               │ ▲                    ▲
                           │               │ │                    │
                           │               ▼ │                    │
                           │        ┌─────────────┐               │
                           └───────▶│ Local Files │───────────────┘
                                    │   Folder    │
                                    └─────────────┘
```

## Technology Stack

- **HTTP API**: Standard Go net/http for REST endpoints
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
    Items []SyncItem `json:"items"`
}

type SyncItem struct {
    From            string `json:"from"`
    To              string `json:"to"`
    DeleteAfterSync bool   `json:"delete_after_sync,omitempty"`
    Overwrite       bool   `json:"overwrite,omitempty"`
}

type SyncResult struct {
    Items         []SyncItemResult `json:"items"`
    TotalFiles    int              `json:"total_files"`
    UploadedFiles int              `json:"uploaded_files"`
    FailedFiles   []string         `json:"failed_files"`
    Duration      string           `json:"duration"`
}

type SyncItemResult struct {
    From          string   `json:"from"`
    To            string   `json:"to"`
    TotalFiles    int      `json:"total_files"`
    UploadedFiles int      `json:"uploaded_files"`
    FailedFiles   []string `json:"failed_files"`
}
```

### Configuration Structure (pkg/config)

```go
type Config struct {
    Redis   RedisConfig   `toml:"redis" validate:"required"`
    S3      S3Config      `toml:"s3" validate:"required"`
    Daemon  DaemonConfig  `toml:"daemon" validate:"required"`
    Publish PublishConfig `toml:"publish" validate:"required"`
    HTTP    HTTPConfig    `toml:"http" validate:"required"`
    Cache   CacheConfig   `toml:"cache" validate:"required"`
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
    LogLevel           string `toml:"log_level" validate:"required,oneof=debug info warn error fatal"`
    SSHCommand         string `toml:"ssh_command"`
    SSHDebounceMinutes int    `toml:"ssh_debounce_minutes" validate:"min=1"`
    SSHTimeoutMinutes  int    `toml:"ssh_timeout_minutes" validate:"min=1"`
    EnableSSHTask      bool   `toml:"enable_ssh_task"`
}

type PublishConfig struct {
    MaxRetry       int `toml:"max_retry" validate:"required,min=0,max=10"`
    TimeoutMinutes int `toml:"timeout_minutes" validate:"required,min=1,max=1440"`
}

type HTTPConfig struct {
    Addr string `toml:"addr" validate:"required,hostname_port"`
}

type CacheConfig struct {
    MaxConcurrentS3Checks int      `toml:"max_concurrent_s3_checks" validate:"min=1,max=100"`
    AllowedPrefixes       []string `toml:"allowed_prefixes" validate:"required,min=1"`
}
```

## Core Component Design

### 1. File Sync Handler (pkg/handler/FileSyncHandler)

- Responsible for handling file synchronization tasks
- Supports intelligent file skipping (based on file size comparison)
- Optional forced overwrite mode (configurable per sync item via `overwrite`)
- Cross-platform S3 path handling (Windows backslash conversion)
- Context cancellation support
- Optional file deletion after successful sync (configurable per sync item via `delete_after_sync`)
- Automatic cleanup of empty directories after file deletion

### 2. HTTP Handler (pkg/http/HTTPHandler)

- Provides REST API endpoint for task submission
- Validates HTTP request payload (from, to, and optional delete_after_sync and overwrite)
- Uses Publisher to create and push tasks to Redis queue
- Returns JSON responses with success/error status
- Provides file existence check endpoint with Redis caching

### 3. Task Publisher (pkg/publisher/Publisher)

- Validates local folder existence and non-emptiness
- Creates and pushes tasks to Redis queue
- Supports retry and timeout configuration

### 4. Daemon Service (internal/daemon/DaemonService)

- HTTP server management for API endpoints
- Asynq server management for task processing
- Graceful shutdown support for both servers

### 5. File Existence Cache (pkg/cache/FileExistenceCache)

- Redis-backed caching for S3 file existence checks
- Reduces S3 API calls and improves response times
- Automatic cache updates when files are uploaded
- Thread-safe concurrent access with in-memory locking
- Security features: input validation, prefix restrictions

### 6. Logging System (pkg/logger/Logger)

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
  - HTTP address: :8080
  - Log level: info
  - Max retry: 3
  - Timeout: 30 minutes
  - Max concurrent S3 checks: 10
  - Allowed prefixes: ["store/"]

## Directory Structure

```
sync4loong/
├── cmd/
│   └── daemon/            # Daemon process with HTTP API
├── pkg/
│   ├── config/            # Configuration management (with validation)
│   ├── task/              # Task definitions
│   ├── handler/           # File sync handler
│   ├── publisher/         # Task publisher
│   ├── http/              # HTTP API handlers
│   ├── cache/             # File existence cache with Redis
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

1. HTTP client sends POST request to `/publish` endpoint with JSON payload
2. HTTP handler validates request payload (from, to, and optional delete_after_sync and overwrite)
3. Publisher checks local folder existence and non-emptiness
4. Creates `FileSyncPayload` task payload
5. Pushes task to Redis queue
6. Returns JSON response with success/error status

### 2. Task Execution Flow

1. Daemon retrieves tasks from Redis queue
2. Recursively traverses all files in the specified folder
3. Generates S3 key for each file: `prefix + relative path`
4. Executes intelligent upload decision:
   - `overwrite` is true → Upload (force overwrite)
   - File doesn't exist → Upload
   - File size differs → Overwrite upload
   - File size same → Skip
5. Records detailed upload logs and results
6. Optionally deletes source files after successful upload (if `delete_after_sync` is enabled)
7. Automatically cleans up empty directories after file deletion

### 3. File Existence Check Flow

1. HTTP client sends GET request to `/check/{key}` or `/check?key={s3_key}`
2. System validates S3 key format and checks against allowed prefixes
3. Check Redis cache for existing entry:
   - **Cache Hit**: Return cached result immediately
   - **Cache Miss**: Proceed to S3 check with in-memory lock to prevent duplicate requests
4. Query S3 HeadObject API to check file existence
5. Store result in Redis cache (persistent until manually updated)
6. Return JSON response with existence status, cache info, and timestamp

### 4. Cache Update Flow

1. When `FileSyncHandler` successfully uploads a file:
   - Immediately update cache: `file_exists:{bucket}:{s3_key} = {"exists": true, "size": fileSize, "timestamp": now}`
2. Cache invalidation strategies:
   - **Manual update**: Cache is updated when files are uploaded or modified
   - **Manual invalidation**: If file deletion is supported in the future

### 5. File Path Mapping

```
Local: /data/nix-store/bin/bash
S3:    store/bin/bash

Local: /data/nix-store/lib/libc.so.6
S3:    store/lib/libc.so.6
```

## API Endpoints

### Task Publishing

**Endpoint**: `POST /publish`

**Request**:
```bash
curl -X POST "http://localhost:8080/publish" \
  -H "Content-Type: application/json" \
  -d '[
    {"from": "/data/nix-store", "to": "store/", "delete_after_sync": true, "overwrite": false},
    {"from": "/data/single-file.txt", "to": "store/file.txt", "delete_after_sync": false, "overwrite": true}
  ]'
```

**Response**:
```json
{
  "success": true,
  "message": "task published successfully"
}
```

**Error Response**:
```json
{
  "success": false,
  "error": "'from' field is required for item 0"
}
```

### File Existence Check

**Endpoint**: `GET /check/{key}` or `GET /check?key={s3_key}`

**Request**:
```bash
curl -X GET "http://localhost:8080/check/store/bin/bash"
# or
curl -X GET "http://localhost:8080/check?key=store/bin/bash"
```

**Response**:
```json
{
  "exists": true,
  "cached": true,
  "timestamp": "2024-01-01T12:00:00Z",
  "size": 1024,
  "s3_key": "store/bin/bash"
}
```

**Error Response**:
```json
{
  "error": "key not allowed: must start with allowed prefix",
  "allowed_prefixes": ["store/", "nix/"]
}
```

### Security and Performance Features

**Input Validation**:
- S3 key format validation (no path traversal, special characters)
- Prefix whitelist enforcement (e.g., only allow `store/`, `nix/` prefixes)
- Maximum key length limits

**Caching Strategy**:
- **Persistent cache**: Files cached indefinitely until manually updated
- **Positive cache**: File exists (with size and timestamp info)
- **Negative cache**: File doesn't exist (boolean flag)
- **In-memory locking**: Prevent duplicate S3 requests for the same key

**Error Handling**:
- Redis unavailable: Fallback to direct S3 query (no caching)
- S3 unavailable: Return error with proper HTTP status code
- Timeout handling: Configurable timeouts for S3 operations

## Configuration Example

```toml
[cache]
max_concurrent_s3_checks = 10
allowed_prefixes = ["store/", "nix/"]
```
