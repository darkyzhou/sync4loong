.PHONY: build run clean test fmt lint deps

BUILD_DIR=bin
DAEMON_BINARY=daemon
LDFLAGS=-ldflags="-s -w"

build:
	@echo "Building daemon..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(DAEMON_BINARY) ./cmd/daemon

run:
	@echo "Running daemon..."
	go run ./cmd/daemon --config ./config.toml

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
