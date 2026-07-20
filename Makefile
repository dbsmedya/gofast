.PHONY: build build-cli clean test run-cli deps lint

# Binary names
CLI_BINARY=gofast-cli
BIN_DIR=bin

# Build directories
BUILD_DIR=build

# Default target
all: build

# Create bin directory
$(BIN_DIR):
	mkdir -p $(BIN_DIR)

# Download dependencies
deps:
	go mod download
	go mod tidy

# Build CLI
build-cli: $(BIN_DIR)
	go build -o $(BIN_DIR)/$(CLI_BINARY) ./cmd/cli

# Build
build: build-cli

# Clean build artifacts
clean:
	rm -rf $(BIN_DIR) $(BUILD_DIR)
	go clean

# Run tests
test:
	go test -v ./...

# Run CLI with example config
run-cli: build-cli
	./$(BIN_DIR)/$(CLI_BINARY) --config config.yaml stats

# Parse logs with CLI
parse: build-cli
	./$(BIN_DIR)/$(CLI_BINARY) --config config.yaml parse -v

# Development mode with hot reload (requires air)
dev-cli:
	air -c .air-cli.toml

# Install binaries to GOPATH/bin
install: build
	go install ./cmd/cli

# Lint code (requires golangci-lint)
lint:
	golangci-lint run ./...

# Format code
fmt:
	go fmt ./...

# Check for vulnerabilities
vuln:
	govulncheck ./...

# Show help
help:
	@echo "Available targets:"
	@echo "  deps       - Download dependencies"
	@echo "  build      - Build CLI binary"
	@echo "  build-cli  - Build CLI binary only"
	@echo "  clean      - Remove build artifacts"
	@echo "  test       - Run tests"
	@echo "  run-cli    - Run CLI with example config"
	@echo "  parse      - Parse logs with CLI"
	@echo "  install    - Install binaries to GOPATH/bin"
	@echo "  fmt        - Format code"
	@echo "  lint       - Run linter"
	@echo "  vuln       - Check for vulnerabilities"
