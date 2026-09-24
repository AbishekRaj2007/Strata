BIN_DIR := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Key distribution for the nightly soak. Zipfian is the default because a hot
# head spreads one key's versions across every level, which is where ordering
# bugs live.
DIST ?= zipfian

.DEFAULT_GOAL := help

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-10s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the server and dev CLI into bin/
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/strata-server ./cmd/strata-server
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/strata-cli ./cmd/strata-cli

# T9.3's release targets. linux/amd64 and linux/arm64 only, per CLAUDE.md
# §8 ("Do not add Windows compatibility layers") and plan.md's "Linux only".
DIST_DIR := $(BIN_DIR)/dist
DIST_PLATFORMS := linux/amd64 linux/arm64

.PHONY: dist
dist: ## Cross-compile static release binaries for linux/amd64 and linux/arm64 into bin/dist/
	@for p in $(DIST_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=$(DIST_DIR)/$$os-$$arch; \
		echo "building $$out/strata-server, $$out/strata-cli"; \
		mkdir -p $$out; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out/strata-server ./cmd/strata-server || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out/strata-cli ./cmd/strata-cli || exit 1; \
	done

.PHONY: docker-build
docker-build: ## Build the strata-server Docker image tagged with the current version
	docker build --build-arg VERSION=$(VERSION) -t strata-server:$(VERSION) .

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

.PHONY: wal-fuzz
wal-fuzz: ## Fuzz the WAL framing reader for 60s, the T2.3 done-when condition
	go test ./internal/wal -run '^$$$$' -fuzz FuzzFramingReader -fuzztime 60s

.PHONY: wal-progress
wal-progress: ## Count remaining T2.1/T2.3 failures
	@# The memory cap turns a non-advancing reader into a failure, not an OOM kill.
	@bash -c 'ulimit -v 4000000; go test ./internal/wal -short 2>&1 \
		| grep -cE "^(---|    ---) FAIL" || true' \
		| xargs -I{} echo "{} failing (54 at the start of T2.1)"

.PHONY: baseline
baseline: build ## Run the redis-benchmark baseline and print a benchmarks.md table
	test/bench/baseline.sh

.PHONY: profile
profile: build ## Capture CPU and heap profiles under load into docs/profiles/
	test/bench/profile.sh

.PHONY: full-bench
full-bench: build ## Run T8.1's full workload suite against the durable engine
	test/bench/full.sh

# Not part of "test": the go-redis half is a nested module and needs network
# access on first run. See test/interop/README.md.
.PHONY: interop
interop: build ## Drive the server with redis-cli and go-redis (T1.3 done-when)
	test/interop/run.sh

.PHONY: crash
crash: ## Run the 100-iteration kill/restart durability suite (T2.4 done-when)
	STRATA_DURABLE=1 go test ./test/crash/... -v -timeout 20m

.PHONY: model
model: ## Run the per-commit model test (100k operations)
	go test ./test/model -timeout 30m

.PHONY: model-soak
model-soak: ## Run the nightly 10M-operation model soak (T7.1 done-when)
	go test ./test/model -run TestModelSoak -v -timeout 6h \
		-model.ops=10000000 -model.dist=$(DIST)

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
