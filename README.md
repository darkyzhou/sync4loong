# Sync4Loong

A file synchronization system built with Go and Asynq for the nix4loong CI infrastructure. It synchronizes local folders to S3-compatible storage through a distributed task queue.

## Components

- **daemon**: Background worker that processes sync tasks and uploads files to S3, includes HTTP API for task submission

## Quick Start

Build the project:

```bash
make all
```

Start the daemon:

```bash
make run-daemon
```

Publish sync tasks via HTTP API:

```bash
curl -X POST http://localhost:8080/publish \
  -H "Content-Type: application/json" \
  -d '[
    {"from": "/path/to/folder", "to": "store/", "delete_after_sync": true},
    {"from": "/path/to/file", "to": "store/file", "delete_after_sync": false}
  ]'
```

Check if a file exists in S3 (with Redis caching):

```bash
# Using path parameter
curl -X GET http://localhost:8080/check/store/bin/bash

# Using query parameter  
curl -X GET "http://localhost:8080/check?key=store/bin/bash"
```
