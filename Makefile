.PHONY: all build clean test fmt lint deps
.PHONY: run-daemon run-publish
.PHONY: build-all build-publish build-daemon

BUILD_DIR=bin
PUBLISH_BINARY=publish
DAEMON_BINARY=daemon
LDFLAGS=-ldflags="-s -w"

all: build-all

build-all: build-publish build-daemon

build-publish:
	@echo "Building publish CLI..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(PUBLISH_BINARY) ./cmd/publish

build-daemon:
	@echo "Building daemon..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(DAEMON_BINARY) ./cmd/daemon

run-daemon:
	@echo "Running daemon..."
	go run ./cmd/daemon --config ./config.toml

run-publish:
	@echo "Running publish..."
	go run ./cmd/publish --config ./config.toml $(ARGS)

clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)

test:
	@echo "Running tests..."
	go test -v ./...

fmt:
	@echo "Formatting code..."
	go fmt ./...

lint:
	@echo "Running linter..."
	golangci-lint run

deps:
	@echo "Downloading dependencies..."
	go mod download
	go mod tidy
