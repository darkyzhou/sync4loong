# Sync4Loong

A file synchronization system built with Go and Asynq for the nix4loong CI infrastructure. It synchronizes local folders to S3-compatible storage through a distributed task queue.

## Components

- **publish**: CLI tool for submitting file sync tasks to the queue
- **daemon**: Background worker that processes sync tasks and uploads files to S3

## Quick Start

Build the project:

```bash
make all
```

Start the daemon:

```bash
make run-daemon
```

Publish a sync task:

```bash
make run-publish ARGS="--prefix ... --folder ..."
```
