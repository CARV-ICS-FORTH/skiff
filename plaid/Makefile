.PHONY: all build vet test test-race clean

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
# -s -w drops the symbol table and DWARF (about 30% of the binary); stack traces are unaffected.
LDFLAGS := -s -w -X plaid/pkg/version.Version=$(VERSION) \
           -X plaid/pkg/version.BuildTime=$(BUILD_TIME) \
           -X plaid/pkg/version.GitCommit=$(GIT_COMMIT)

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidd ./cmd/plaidd
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaid ./cmd/plaid
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidctl ./cmd/plaidctl
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidtainer ./cmd/plaidtainer

vet:
	go vet ./...

test:
	go test -v ./pkg/... ./cmd/...

test-race:
	go test -race -v ./pkg/... ./cmd/...

clean:
	rm -rf $(BIN_DIR)
