# Readest → BookOrbit bridge build automation.

BINARY   ?= bridge
CMD      := ./cmd/bridge
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
BUILD_DIR ?= bin

# Build a static Linux binary: no CGO, stripped debug info, embedded version.
GOFLAGS   ?= -trimpath
LDFLAGS   := -s -w -X main.version=$(VERSION)
CGO_ENABLED ?= 0

.PHONY: all build run test vet lint fmt tidy clean help

all: build ## Default target: build the binary.

build: ## Build the static binary into $(BUILD_DIR).
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BINARY) $(CMD)

run: ## Run the CLI directly (requires configs/bridge.yaml or env vars).
	go run $(CMD) --config configs/bridge.yaml

test: ## Run all unit tests.
	go test ./...

vet: ## Run go vet across the module.
	go vet ./...

fmt: ## Format all Go source with gofmt.
	gofmt -s -w .

lint: vet ## Alias for static checks (extend with golangci-lint if installed).

tidy: ## Tidy module dependencies.
	go mod tidy

clean: ## Remove build artifacts.
	rm -rf $(BUILD_DIR)

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
