# The version lives in internal/version; `make build VERSION=1.2.3` overrides it.
LDFLAGS := -s -w
ifdef VERSION
LDFLAGS += -X github.com/biga-app/cli/internal/version.version=$(VERSION)
endif

.PHONY: build install test lint fmt tidy snapshot proto

build: ## Build ./biga
	go build -trimpath -ldflags "$(LDFLAGS)" -o biga ./cmd/biga

install: ## Install biga into $GOBIN
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/biga

test: ## Run the tests with the race detector
	go test -race ./...

proto: ## Regenerate internal/api/apipb from ../proto
	buf generate

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format the code
	golangci-lint fmt ./...

tidy: ## Tidy go.mod and go.sum
	go mod tidy

snapshot: ## Build every release target into dist/ without publishing
	goreleaser release --snapshot --clean --skip=sign
