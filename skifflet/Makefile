.PHONY: all build build-linux build-linux-amd64 build-linux-arm64 vet test test-race clean

BIN_DIR := bin
BINARY := $(BIN_DIR)/skifflet

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
LDFLAGS := -X skifflet/pkg/version.Version=$(VERSION) \
           -X skifflet/pkg/version.BuildTime=$(BUILD_TIME) \
           -X skifflet/pkg/version.GitCommit=$(GIT_COMMIT) \
           -X skifflet/cmd/skifflet/commands.BuildVersion=$(VERSION) \
           -X skifflet/cmd/skifflet/commands.BuildTime=$(BUILD_TIME) \
           -X skifflet/cmd/skifflet/commands.GitCommit=$(GIT_COMMIT)

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/skifflet

build-linux: build-linux-amd64

build-linux-amd64:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-amd64 ./cmd/skifflet
	@cp -f $(BINARY)-linux-amd64 $(BINARY)-linux 2>/dev/null || true

build-linux-arm64:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-arm64 ./cmd/skifflet

vet:
	go vet ./...

test:
	go test -v ./pkg/... ./internal/... ./cmd/...

test-race:
	go test -race -v ./pkg/... ./internal/... ./cmd/...

clean:
	rm -rf $(BIN_DIR)
