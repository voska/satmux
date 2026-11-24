VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo "")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
BIN     := bin/satmux
GOFUMPT       := .tools/gofumpt
GOIMPORTS     := .tools/goimports
GOLANGCI_LINT := .tools/golangci-lint

.PHONY: build fmt fmt-check lint test ci tools clean

build:
	@mkdir -p bin
	@go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/satmux

fmt: tools
	@$(GOIMPORTS) -local github.com/voska/satmux -w .
	@$(GOFUMPT) -w .

fmt-check: tools
	@$(GOIMPORTS) -local github.com/voska/satmux -w .
	@$(GOFUMPT) -w .
	@git diff --exit-code -- '*.go' go.mod go.sum

lint: tools
	@$(GOLANGCI_LINT) run

test:
	@go test -race -vet=off ./...

ci: fmt-check lint test build

tools:
	@mkdir -p .tools
	@GOBIN=$(CURDIR)/.tools go install mvdan.cc/gofumpt@latest
	@GOBIN=$(CURDIR)/.tools go install golang.org/x/tools/cmd/goimports@latest
	@GOBIN=$(CURDIR)/.tools go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

clean:
	rm -rf bin/
