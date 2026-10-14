# Makefile for Skiff project

REGISTRY ?= docker.io/carvicsforth
IMAGE ?= $(REGISTRY)/skiff

# The platform the image is built for (linux/amd64 or linux/arm64); the host's by default.
# Everything is built in containers, so any platform can be built on any host.
PLATFORM ?=
PLATFORM_FLAG := $(if $(PLATFORM),--platform $(PLATFORM))
SAVE_FLAG := $(if $(PLATFORM),--platform $(PLATFORM))

BASE_VERSION ?= $(shell cat VERSION 2>/dev/null || echo "0.1.0")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
# A version between releases is VERSION and the first 7 hexadecimal digits of the commit.
GIT_COMMIT_SHORT ?= $(or $(shell git rev-parse HEAD 2>/dev/null | cut -c1-7),unknown)
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

BUILD_ARGS = --build-arg VERSION="$(VERSION)" --build-arg GIT_COMMIT="$(GIT_COMMIT)" --build-arg BUILD_TIME="$(BUILD_TIME)"

export GOFLAGS ?= -buildvcs=false

.PHONY: all build build-skifflet build-plaid vet test test-race clean builder image publish develop e2e-images fmt fmt-check check-shell

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

# The race detector needs cgo and the Linux-only tests (terminals, namespaces, process
# trees) need Linux, so this runs in the builder container: the same pinned toolchain on
# every machine. It runs as an unprivileged user (tests that depend on directory modes skip
# as root); --init reaps the processes the tests kill and --privileged lets the network
# namespace tests create namespaces.
test-race: builder
	docker run --rm --init --privileged --user 1000:1000 -e GOFLAGS \
		-e HOME=/home/ubuntu -e GOPATH=/home/ubuntu/.go \
		-v "$(CURDIR)":/src \
		-v skiff-test-go-mod:/home/ubuntu/.go/pkg/mod -v skiff-test-go-build:/home/ubuntu/.cache/go-build \
		$(IMAGE):builder \
		sh -c 'make -C skifflet test-race && make -C plaid test-race'

fmt:
	@echo "Formatting Go source files..."
	gofmt -w skifflet plaid

fmt-check:
	@test -z "$$(gofmt -l skifflet plaid)" || (echo "Unformatted Go files found:" && gofmt -l skifflet plaid && exit 1)

# Every shell script of the repository, checked by the builder's shellcheck (a missing one
# is a failure, not a skip). Warnings count, except SC2034 (a variable that is set and not
# used here: mostly names that are sourced or exported, which shellcheck cannot see).
check-shell: builder
	docker run --rm -v "$(CURDIR)":/src:ro $(IMAGE):builder \
		sh -c 'shellcheck --severity=warning --exclude=SC2034 $$(find scripts images test -type f \( -name "*.sh" -o -name "*.slurm" \) -not -path "*/.*" -not -path "*/results/*")'


# The build and test environment: the first stage of the one Skiff image's Dockerfile
# (tagged skiff:builder, kept locally, never published).
builder:
	@echo "Building the builder stage of the skiff image locally..."
	docker buildx build $(PLATFORM_FLAG) --load --target builder \
		-t $(IMAGE):builder \
		-f images/skiff/Dockerfile .

image:
	@echo "Building the skiff image locally (Version: $(VERSION))..."
	docker buildx build $(PLATFORM_FLAG) --load $(BUILD_ARGS) \
		-t $(IMAGE):$(VERSION) \
		-t $(IMAGE):latest \
		-f images/skiff/Dockerfile .

# A release: the image of this commit for both platforms, pushed under its version alone (the
# scripts of a release run the image of their own version; a version that is already published
# is refused, so it never changes), without build metadata; then the package for users in
# dist/skiff-<version>: the README, the license, the scripts and the version they run.
DIST := dist/skiff-$(VERSION)

publish:
	@test -z "$$(git -c core.fsmonitor=false status --porcelain)" || { echo "publish: the tree has changes; a release is made of a commit" >&2; exit 1; }
	@if docker buildx imagetools inspect $(IMAGE):$(VERSION) >/dev/null 2>&1; then echo "publish: $(IMAGE):$(VERSION) is published already" >&2; exit 1; fi
	@echo "Building and pushing the multi-arch skiff image (Version: $(VERSION))..."
	docker buildx build --platform linux/amd64,linux/arm64 --provenance=false $(BUILD_ARGS) \
		-t $(IMAGE):$(VERSION) \
		--push \
		-f images/skiff/Dockerfile .
	@echo "Making the package $(DIST).tar.gz..."
	rm -rf $(DIST) $(DIST).tar.gz
	mkdir -p $(DIST)/scripts
	cp README.md LICENSE $(DIST)/
	cp scripts/skiff.sh scripts/skiff.slurm $(DIST)/scripts/
	echo "$(VERSION)" > $(DIST)/VERSION
	COPYFILE_DISABLE=1 tar --no-xattrs -C dist -czf $(DIST).tar.gz skiff-$(VERSION)
	@echo "Published $(IMAGE):$(VERSION); the package is $(DIST).tar.gz"

# Build the image and the builder for local development, and deploy them with the scripts
# to the Vagrant VMs. The VMs need no Apptainer, slirp4netns, socat or kubectl of their own:
# `skiff.sh tools` takes them out of the image that was just uploaded, and the Apptainer
# that comes with it converts the image to the SIF the jobs run.
develop: builder image
	@echo "Exporting the image to a tar file..."
	@mkdir -p /tmp/skiff-images
	docker save $(SAVE_FLAG) -o /tmp/skiff-images/skiff.tar $(IMAGE):latest

	@echo "Generating deployment manifest..."
	@if command -v sha256sum >/dev/null 2>&1; then \
		TAR_SHA=$$(sha256sum /tmp/skiff-images/skiff.tar | awk '{print $$1}'); \
	elif command -v shasum >/dev/null 2>&1; then \
		TAR_SHA=$$(shasum -a 256 /tmp/skiff-images/skiff.tar | awk '{print $$1}'); \
	else \
		echo "Error: neither sha256sum nor shasum found" >&2; exit 1; \
	fi; \
	GIT_DIRTY=$$(test -z "$$(git -c core.fsmonitor=false status --porcelain)" && echo false || echo true); \
	IMAGE_ID=$$(docker image inspect --format '{{.Id}}' $(IMAGE):latest); \
	IMAGE_PLATFORM=$$(docker image inspect --format '{{.Os}}/{{.Architecture}}' $(IMAGE):latest); \
	BUILDER_ID=$$(docker image inspect --format '{{.Id}}' $(IMAGE):builder); \
	GO_VERSION=$$(docker run --rm $(IMAGE):builder go version | awk '{print $$3}'); \
	printf '{\n  "version": "%s",\n  "git_commit": "%s",\n  "git_dirty": %s,\n  "build_time": "%s",\n  "tar_sha256": "%s",\n  "image_id": "%s",\n  "platform": "%s",\n  "builder_image_id": "%s",\n  "go_version": "%s"\n}\n' \
		"$(VERSION)" "$(GIT_COMMIT)" "$$GIT_DIRTY" "$(BUILD_TIME)" "$$TAR_SHA" "$$IMAGE_ID" "$$IMAGE_PLATFORM" "$$BUILDER_ID" "$$GO_VERSION" > /tmp/skiff-images/manifest.json

	@echo "Copying image, manifest and scripts to the Vagrant VMs..."
	test/vagrant/transport.sh ssh controller "mkdir -p ~/.skiff/images && rm -f ~/.skiff/images/skiff.sif ~/.skiff/images/skiff-bubble.*"
	test/vagrant/transport.sh upload /tmp/skiff-images/skiff.tar .skiff/images/skiff.tar controller
	test/vagrant/transport.sh upload /tmp/skiff-images/manifest.json .skiff/images/manifest.json controller
	$(MAKE) -C test/vagrant sync

	@echo "Extracting the tools from the image and converting it to a SIF on the controller VM..."
	test/vagrant/transport.sh ssh controller 'bash ~/skiff/scripts/skiff.sh tools ~/.skiff/images/skiff.tar && PATH="$$HOME/.skiff/tools/bin:$$PATH" apptainer build --force ~/.skiff/images/skiff.sif docker-archive://$$HOME/.skiff/images/skiff.tar'

	@echo "Development image and deployment manifest deployed successfully!"
	@echo "Submit with SKIFF_DEV=1 to use the local image."
	@echo "Run 'make e2e-images' to also build and deploy the e2e fixture image."

e2e-images:
	@echo "Building e2e fixture image (Version: $(VERSION))..."

	docker buildx build $(PLATFORM_FLAG) --load --target fixture \
		-t skiff-e2e-fixture:$(GIT_COMMIT_SHORT) \
		-t skiff-e2e-fixture:latest \
		-f test/e2e/images/fixture/Dockerfile test/e2e/images/fixture
	docker buildx build $(PLATFORM_FLAG) --load --target fixture-user \
		-t skiff-e2e-fixture-user:$(GIT_COMMIT_SHORT) \
		-f test/e2e/images/fixture/Dockerfile test/e2e/images/fixture

	@echo "Exporting fixture image to tar..."
	@mkdir -p /tmp/skiff-images
	docker save $(SAVE_FLAG) -o /tmp/skiff-images/skiff-e2e-fixture.tar skiff-e2e-fixture:$(GIT_COMMIT_SHORT)
	docker save $(SAVE_FLAG) -o /tmp/skiff-images/skiff-e2e-fixture-user.tar skiff-e2e-fixture-user:$(GIT_COMMIT_SHORT)

	@echo "Uploading fixture tar to controller VM..."
	test/vagrant/transport.sh ssh controller "mkdir -p ~/.skiff/images"
	test/vagrant/transport.sh upload /tmp/skiff-images/skiff-e2e-fixture.tar .skiff/images/skiff-e2e-fixture.tar controller
	test/vagrant/transport.sh upload /tmp/skiff-images/skiff-e2e-fixture-user.tar .skiff/images/skiff-e2e-fixture-user.tar controller

	@echo "Converting fixture tar to SIF on controller VM..."
	test/vagrant/transport.sh ssh controller \
		'export PATH="$$HOME/.skiff/tools/bin:$$PATH"; apptainer build --force ~/.skiff/images/skiff-e2e-fixture.sif docker-archive://$$HOME/.skiff/images/skiff-e2e-fixture.tar && apptainer build --force ~/.skiff/images/skiff-e2e-fixture-user.sif docker-archive://$$HOME/.skiff/images/skiff-e2e-fixture-user.tar'

	@echo "Writing fixture env file..."
	@printf 'export FIXTURE_IMAGE_TAG="%s"\nexport FIXTURE_IMAGE="/var/lib/skiff/images/skiff-e2e-fixture.sif"\nexport FIXTURE_USER_IMAGE="/var/lib/skiff/images/skiff-e2e-fixture-user.sif"\n' \
		"$(GIT_COMMIT_SHORT)" > /tmp/skiff-images/fixture.env
	test/vagrant/transport.sh upload /tmp/skiff-images/fixture.env .skiff/fixture.env controller

	@echo "e2e fixture image deployed. Tag: $(GIT_COMMIT_SHORT)"
	@echo "Run 'make -C test/vagrant e2e' to qualify the deployed images."

clean:
	$(MAKE) -C skifflet clean
	$(MAKE) -C plaid clean
	rm -rf /tmp/skiff-images
