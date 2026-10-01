FROM ubuntu:24.04

# The build and test environment of every candidate. The Go toolchain is an exact,
# checksummed release (as the K3s and CNI plugins are in the bubble image), so a
# change of toolchain is a change of this file and cannot be mistaken for a code
# regression. gcc and the C library headers are here for the race detector (cgo);
# the Skiff binaries themselves are built with CGO_ENABLED=0. shellcheck is here so that
# the scripts are checked by the same version everywhere, and cannot be skipped.
ARG TARGETARCH
ARG GO_VERSION=1.26.8
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl make gcc libc6-dev shellcheck && \
    if [ "${TARGETARCH}" = "amd64" ]; then \
        GO_SHA="d0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b"; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
        GO_SHA="211ffced9dcb9633a55eac6364816ec0ddd951389a740e88fa8b3337971bdda0"; \
    else \
        echo "Unsupported architecture: ${TARGETARCH}" && exit 1; \
    fi && \
    curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" && \
    echo "${GO_SHA}  /tmp/go.tgz" | sha256sum -c - && \
    tar -C /usr/local -xzf /tmp/go.tgz && \
    rm /tmp/go.tgz && \
    rm -rf /var/lib/apt/lists/*

# Set Go environment variables
ENV GOROOT=/usr/local/go
ENV GOPATH=/root/.go
ENV PATH=$PATH:$GOROOT/bin:$GOPATH/bin

# The image's unprivileged user (ubuntu, 1000) runs the tests: some of them (directory modes, user namespaces)
# say nothing as root. Its Go caches are in the image so that volumes mounted over them
# start with the right owner. Builds keep running as root.
RUN mkdir -p /home/ubuntu/.go/pkg/mod /home/ubuntu/.cache/go-build && \
    chown -R ubuntu:ubuntu /home/ubuntu

WORKDIR /src
