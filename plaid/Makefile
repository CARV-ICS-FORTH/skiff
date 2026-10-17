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

# plaidd is built with cgo for its InfiniBand transport (pkg/rdma) and linked statically, with
# rdma-core's libibverbs and mlx5 provider from the builder, so that it needs nothing of the
# host's but the kernel: netgo and osusergo keep Go's own DNS resolver and user lookup, as
# without cgo. The linker warns that getaddrinfo and getservbyname need the host's C library
# in a static program: they are libnl's, which plaidd never calls. The other programs are
# pure Go.
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 go build -tags netgo,osusergo -ldflags "$(LDFLAGS) -linkmode external -extldflags -static" -o $(BIN_DIR)/plaidd ./cmd/plaidd
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaid ./cmd/plaid
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidctl ./cmd/plaidctl
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/plaidtainer ./cmd/plaidtainer

vet:
	go vet ./...

test:
	go test -v ./pkg/... ./cmd/...

test-race:
	go test -race -v ./pkg/... ./cmd/...

clean:
	rm -rf $(BIN_DIR)
