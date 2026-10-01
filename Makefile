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

export GOFLAGS ?= -buildvcs=false

.PHONY: all build build-skifflet build-plaid vet test test-race clean builder images publish-images develop e2e-images fmt fmt-check check-shell

all: build

build: build-skifflet build-plaid

build-skifflet:
	@echo "Building skifflet..."
	$(MAKE) -C skifflet build

build-plaid:
	@echo "Building plaid..."
	$(MAKE) -C plaid build

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
	bash test/regression.sh

# The race detector needs cgo and the Linux-only tests (terminals, namespaces,
# process trees) need Linux, so this runs in the builder container: the same pinned
# toolchain on every machine. It runs as an unprivileged user (tests that depend on
# directory modes skip as root); --init reaps the processes the tests kill and
# --privileged lets the network namespace tests create namespaces.
test-race: builder
	docker run --rm --init --privileged --user 1000:1000 -e GOFLAGS \
		-e HOME=/home/ubuntu -e GOPATH=/home/ubuntu/.go \
		-v "$(CURDIR)":/src \
		-v skiff-test-go-mod:/home/ubuntu/.go/pkg/mod -v skiff-test-go-build:/home/ubuntu/.cache/go-build \
		$(REGISTRY)/skiff-builder:latest \
		sh -c 'make -C skifflet test-race && make -C plaid test-race'

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
	GIT_DIRTY=$$(test -z "$$(git -c core.fsmonitor=false status --porcelain)" && echo false || echo true); \
	IMAGE_ID=$$(docker image inspect --format '{{.Id}}' $(REGISTRY)/skiff-bubble:latest); \
	BUILDER_ID=$$(docker image inspect --format '{{.Id}}' $(REGISTRY)/skiff-builder:latest); \
	GO_VERSION=$$(docker run --rm $(REGISTRY)/skiff-builder:latest go version | awk '{print $$3}'); \
	printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "git_dirty": %s,\n  "build_time": "%s",\n  "tar_sha256": "%s",\n  "image_id": "%s",\n  "builder_image_id": "%s",\n  "go_version": "%s"\n}\n' \
		"$(VERSION)" "$(GIT_COMMIT)" "$$GIT_DIRTY" "$(BUILD_TIME)" "$$TAR_SHA" "$$IMAGE_ID" "$$BUILDER_ID" "$$GO_VERSION" > /tmp/skiff-images/manifest.json

	@echo "Copying image and manifest to Vagrant VMs..."
	test/vagrant/transport.sh ssh controller "mkdir -p ~/.skiff/images && rm -f ~/.skiff/images/skiff-bubble.sif"
	test/vagrant/transport.sh upload /tmp/skiff-images/skiff-bubble.tar /home/vagrant/.skiff/images/skiff-bubble.tar controller
	test/vagrant/transport.sh upload /tmp/skiff-images/manifest.json /home/vagrant/.skiff/images/manifest.json controller

	@echo "Converting tar to SIF on controller VM..."
	test/vagrant/transport.sh ssh controller "apptainer build --force ~/.skiff/images/skiff-bubble.sif docker-archive:///home/vagrant/.skiff/images/skiff-bubble.tar"

	$(MAKE) -C test/vagrant sync

	@echo "Development image and deployment manifest deployed successfully!"
	@echo "Submit with SKIFF_DEV=1 to use the local image."
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
	test/vagrant/transport.sh ssh controller "mkdir -p ~/.skiff/images"
	test/vagrant/transport.sh upload /tmp/skiff-images/skiff-e2e-fixture.tar /home/vagrant/.skiff/images/skiff-e2e-fixture.tar controller

	@echo "Converting fixture tar to SIF on controller VM..."
	test/vagrant/transport.sh ssh controller \
		"apptainer build --force ~/.skiff/images/skiff-e2e-fixture.sif docker-archive:///home/vagrant/.skiff/images/skiff-e2e-fixture.tar"

	@echo "Writing fixture env file..."
	@printf 'export FIXTURE_IMAGE_TAG="%s"\nexport FIXTURE_IMAGE="/var/lib/skiff/images/skiff-e2e-fixture.sif"\n' \
		"$(GIT_COMMIT_SHORT)" > /tmp/skiff-images/fixture.env
	test/vagrant/transport.sh upload /tmp/skiff-images/fixture.env /home/vagrant/.skiff/fixture.env controller

	@echo "e2e fixture image deployed. Tag: $(GIT_COMMIT_SHORT)"
	@echo "Run 'make -C test/vagrant e2e' to qualify the deployed images."

clean:
	$(MAKE) -C skifflet clean
	$(MAKE) -C plaid clean
	rm -rf /tmp/skiff-images
