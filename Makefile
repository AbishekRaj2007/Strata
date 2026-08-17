BIN_DIR := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-10s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the server and dev CLI into bin/
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/strata-server ./cmd/strata-server
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/strata-cli ./cmd/strata-cli

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: race
race: ## Run all tests under the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests and report per-package coverage
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: bench
bench: ## Run all benchmarks without running tests
	go test -run '^$$' -bench . -benchmem ./...

.PHONY: fuzz
fuzz: ## Fuzz the RESP reader for 60s, the T1.1 done-when condition
	go test ./internal/resp -run '^$$' -fuzz FuzzReadValue -fuzztime 60s

.PHONY: baseline
baseline: build ## Run the redis-benchmark baseline and print a benchmarks.md table
	test/bench/baseline.sh

.PHONY: profile
profile: build ## Capture CPU and heap profiles under load into docs/profiles/
	test/bench/profile.sh

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -s -w .

.PHONY: check
check: vet test ## Run the checks CI runs on every push

.PHONY: clean
clean: ## Remove build output and profiles
	rm -rf $(BIN_DIR) coverage.out
	rm -f *.prof
