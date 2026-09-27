# Makefile for Skiff project

REGISTRY ?= docker.io/chazapis

BASE_VERSION ?= $(shell cat VERSION 2>/dev/null || echo "0.1.0")
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

BUILD_TIME := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

# Binary output directory
BIN_DIR = bin
export GOFLAGS ?= -buildvcs=false

.PHONY: all build build-skifflet build-plaid binaries-linux-amd64 binaries-linux-arm64 vet test test-race clean builder images publish-images develop e2e-images fmt fmt-check check-shell

all: build

build: build-skifflet build-plaid

build-skifflet:
	@echo "Building skifflet..."
	$(MAKE) -C skifflet build

build-plaid:
	@echo "Building plaid..."
	$(MAKE) -C plaid build

binaries-linux-amd64:
	@echo "Building linux/amd64 binaries..."
	@mkdir -p $(BIN_DIR)
	$(MAKE) -C skifflet build-linux-amd64
	$(MAKE) -C plaid build-linux-amd64
	@cp -f skifflet/bin/*-linux-amd64 $(BIN_DIR)/ 2>/dev/null || true
	@cp -f plaid/bin/*-linux-amd64 $(BIN_DIR)/ 2>/dev/null || true

binaries-linux-arm64:
	@echo "Building linux/arm64 binaries..."
	@mkdir -p $(BIN_DIR)
	$(MAKE) -C skifflet build-linux-arm64
	$(MAKE) -C plaid build-linux-arm64
	@cp -f skifflet/bin/*-linux-arm64 $(BIN_DIR)/ 2>/dev/null || true
	@cp -f plaid/bin/*-linux-arm64 $(BIN_DIR)/ 2>/dev/null || true

vet:
	@echo "Running go vet on skifflet..."
	$(MAKE) -C skifflet vet
	@echo "Running go vet on plaid..."
	$(MAKE) -C plaid vet

test:
	@echo "Running skifflet tests..."
	$(MAKE) -C skifflet test
	@echo "Running plaid tests..."
	$(MAKE) -C plaid test

test-race:
	@echo "Running skifflet tests with race detector..."
	$(MAKE) -C skifflet test-race
	@echo "Running plaid tests with race detector..."
	$(MAKE) -C plaid test-race

fmt:
	@echo "Formatting Go source files..."
	gofmt -w skifflet plaid

fmt-check:
	@test -z "$$(gofmt -l skifflet plaid)" || (echo "Unformatted Go files found:" && gofmt -l skifflet plaid && exit 1)

check-shell:
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck --severity=error $$(find scripts images test -type f \( -name "*.sh" -o -name "*.slurm" \) -not -path "*/.*"); \
	else \
		echo "shellcheck not installed, skipping"; \
	fi

builder:
	@echo "Building skiff-builder image locally..."
	docker build \
		-t $(REGISTRY)/skiff-builder:latest \
		-f images/skiff-builder/Dockerfile images/skiff-builder

images:
	@echo "Building skiff-bubble image locally (Version: $(VERSION))..."
	docker build \
		--build-arg REGISTRY=$(REGISTRY) \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		--build-arg BUILD_TIME="$(BUILD_TIME)" \
		-t $(REGISTRY)/skiff-bubble:$(VERSION) \
		-t $(REGISTRY)/skiff-bubble:latest \
		-f images/skiff-bubble/Dockerfile .

publish-images:
	@echo "Building and pushing multi-arch skiff-bubble release image (Version: $(VERSION))..."
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg REGISTRY=$(REGISTRY) \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		--build-arg BUILD_TIME="$(BUILD_TIME)" \
		-t $(REGISTRY)/skiff-bubble:$(VERSION) \
		-t $(REGISTRY)/skiff-bubble:latest \
		--push \
		-f images/skiff-bubble/Dockerfile .

develop:
	@echo "Building images for local development (Version: $(VERSION))..."
	
	# Build skiff-builder locally
	docker build --build-arg REGISTRY=$(REGISTRY) \
		-t $(REGISTRY)/skiff-builder:latest \
		-f images/skiff-builder/Dockerfile images/skiff-builder

	# Build skiff-bubble locally
	docker build --build-arg REGISTRY=$(REGISTRY) \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		--build-arg BUILD_TIME="$(BUILD_TIME)" \
		-t $(REGISTRY)/skiff-bubble:latest \
		-f images/skiff-bubble/Dockerfile .

	@echo "Exporting images to tar files..."
	@mkdir -p /tmp/skiff-images
	docker save -o /tmp/skiff-images/skiff-bubble.tar $(REGISTRY)/skiff-bubble:latest

	@echo "Generating deployment manifest..."
	@if command -v sha256sum >/dev/null 2>&1; then \
		TAR_SHA=$$(sha256sum /tmp/skiff-images/skiff-bubble.tar | awk '{print $$1}'); \
	elif command -v shasum >/dev/null 2>&1; then \
		TAR_SHA=$$(shasum -a 256 /tmp/skiff-images/skiff-bubble.tar | awk '{print $$1}'); \
	else \
		echo "Error: neither sha256sum nor shasum found" >&2; exit 1; \
	fi; \
	printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "build_time": "%s",\n  "tar_sha256": "%s"\n}\n' \
		"$(VERSION)" "$(GIT_COMMIT)" "$(BUILD_TIME)" "$$TAR_SHA" > /tmp/skiff-images/manifest.json

	@echo "Copying image and manifest to Vagrant VMs..."
	cd test/vagrant && vagrant ssh controller -c "mkdir -p ~/.skiff/images && rm -f ~/.skiff/images/skiff-bubble.sif"
	cd test/vagrant && vagrant upload /tmp/skiff-images/skiff-bubble.tar /home/vagrant/.skiff/images/skiff-bubble.tar controller
	cd test/vagrant && vagrant upload /tmp/skiff-images/manifest.json /home/vagrant/.skiff/images/manifest.json controller

	@echo "Converting tar to SIF on controller VM..."
	cd test/vagrant && vagrant ssh controller -c "apptainer build --force ~/.skiff/images/skiff-bubble.sif docker-archive:///home/vagrant/.skiff/images/skiff-bubble.tar"

	@echo "Copying scripts and tests to controller..."
	cd test/vagrant && vagrant ssh controller -c "mkdir -p ~/skiff/scripts ~/skiff/test"
	for f in scripts/*; do \
		(cd test/vagrant && vagrant upload ../../$$f /home/vagrant/skiff/scripts/$$(basename $$f) controller); \
	done
	for f in test/*.sh; do \
		(cd test/vagrant && vagrant upload ../../$$f /home/vagrant/skiff/test/$$(basename $$f) controller); \
	done
	cd test/vagrant && vagrant ssh controller -c "chmod +x ~/skiff/scripts/*.sh ~/skiff/test/*.sh"

	@echo "Copying e2e test suite to controller..."
	cd test/vagrant && vagrant ssh controller -c "mkdir -p ~/skiff/test/e2e/network ~/skiff/test/e2e/lib ~/skiff/test/e2e/images/fixture"
	for f in test/e2e/network/*; do \
		(cd test/vagrant && vagrant upload ../../$$f /home/vagrant/skiff/test/e2e/network/$$(basename $$f) controller); \
	done
	for f in test/e2e/lib/*.sh; do \
		(cd test/vagrant && vagrant upload ../../$$f /home/vagrant/skiff/test/e2e/lib/$$(basename $$f) controller); \
	done
	for f in test/e2e/case-*.sh test/e2e/run-all.sh; do \
		(cd test/vagrant && vagrant upload ../../$$f /home/vagrant/skiff/test/e2e/$$(basename $$f) controller); \
	done
	cd test/vagrant && vagrant ssh controller -c "chmod +x ~/skiff/test/e2e/*.sh ~/skiff/test/e2e/lib/*.sh ~/skiff/test/e2e/network/*.sh"

	@echo "Development image and deployment manifest deployed successfully!"
	@echo "Set SKIFF_DEV=1 in scripts/skiff.slurm to use local image."
	@echo "Run 'make e2e-images' to also build and deploy the e2e fixture image."

e2e-images:
	@echo "Building e2e fixture image (Version: $(VERSION))..."

	docker build \
		-t skiff-e2e-fixture:$(GIT_COMMIT_SHORT) \
		-t skiff-e2e-fixture:latest \
		-f test/e2e/images/fixture/Dockerfile test/e2e/images/fixture

	@echo "Exporting fixture image to tar..."
	@mkdir -p /tmp/skiff-images
	docker save -o /tmp/skiff-images/skiff-e2e-fixture.tar skiff-e2e-fixture:$(GIT_COMMIT_SHORT)

	@echo "Uploading fixture tar to controller VM..."
	cd test/vagrant && vagrant ssh controller -c "mkdir -p ~/.skiff/images"
	cd test/vagrant && vagrant upload /tmp/skiff-images/skiff-e2e-fixture.tar /home/vagrant/.skiff/images/skiff-e2e-fixture.tar controller

	@echo "Converting fixture tar to SIF on controller VM..."
	cd test/vagrant && vagrant ssh controller -c \
		"apptainer build --force ~/.skiff/images/skiff-e2e-fixture.sif docker-archive:///home/vagrant/.skiff/images/skiff-e2e-fixture.tar"

	@echo "Writing fixture env file..."
	@printf 'export FIXTURE_IMAGE_TAG="%s"\nexport FIXTURE_IMAGE="/var/lib/skiff/images/skiff-e2e-fixture.sif"\n' \
		"$(GIT_COMMIT_SHORT)" > /tmp/skiff-images/fixture.env
	cd test/vagrant && vagrant upload /tmp/skiff-images/fixture.env /home/vagrant/.skiff/fixture.env controller

	@echo "e2e fixture image deployed. Tag: $(GIT_COMMIT_SHORT)"
	@echo "Re-run 'make develop' to sync the updated lib/fixture.sh to the VMs."

clean:
	$(MAKE) -C skifflet clean
	$(MAKE) -C plaid clean
	rm -rf $(BIN_DIR) /tmp/skiff-images

