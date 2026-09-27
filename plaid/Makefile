.PHONY: all build build-linux build-linux-amd64 build-linux-arm64 vet test test-race clean

BIN_DIR := bin

BASE_VERSION ?= $(shell cat ../VERSION 2>/dev/null || cat VERSION 2>/dev/null || echo "0.1.0")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
GIT_COMMIT_SHORT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_TAG ?= $(shell git describe --tags --exact-match 2>/dev/null || echo "")

ifeq ($(VERSION),)
ifeq ($(GIT_TAG),$(BASE_VERSION))
    VERSION := $(BASE_VERSION)
else ifeq ($(GIT_TAG),v$(BASE_VERSION))
    VERSION := $(BASE_VERSION)
else
    VERSION := $(BASE_VERSION)-$(GIT_COMMIT_SHORT)
endif
endif

BUILD_TIME ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -X plaid/pkg/version.Version=$(VERSION) \
           -X plaid/pkg/version.BuildTime=$(BUILD_TIME) \
           -X plaid/pkg/version.GitCommit=$(GIT_COMMIT)

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidd ./cmd/plaidd
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaid ./cmd/plaid
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidctl ./cmd/plaidctl
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidtainer ./cmd/plaidtainer

build-linux: build-linux-amd64

build-linux-amd64:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidd-linux-amd64 ./cmd/plaidd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaid-linux-amd64 ./cmd/plaid
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidctl-linux-amd64 ./cmd/plaidctl
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidtainer-linux-amd64 ./cmd/plaidtainer
	@cp -f $(BIN_DIR)/plaidd-linux-amd64 $(BIN_DIR)/plaidd-linux 2>/dev/null || true
	@cp -f $(BIN_DIR)/plaid-linux-amd64 $(BIN_DIR)/plaid-linux 2>/dev/null || true
	@cp -f $(BIN_DIR)/plaidctl-linux-amd64 $(BIN_DIR)/plaidctl-linux 2>/dev/null || true
	@cp -f $(BIN_DIR)/plaidtainer-linux-amd64 $(BIN_DIR)/plaidtainer-linux 2>/dev/null || true

build-linux-arm64:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidd-linux-arm64 ./cmd/plaidd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaid-linux-arm64 ./cmd/plaid
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidctl-linux-arm64 ./cmd/plaidctl
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidtainer-linux-arm64 ./cmd/plaidtainer

vet:
	go vet ./...

test:
	go test -v ./pkg/... ./cmd/...

test-race:
	go test -race -v ./pkg/... ./cmd/...

clean:
	rm -rf $(BIN_DIR)
