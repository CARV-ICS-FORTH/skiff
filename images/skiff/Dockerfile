# syntax=docker/dockerfile:1
#
# The one Skiff image. Its stages:
#   builder  the build and test environment of every candidate (`make builder`, used by
#            test-race and check-shell, never published) and the base of the stages below
#   tools    what a host needs to run Skiff: Apptainer and the programs it runs, slirp4netns,
#            socat and kubectl, in one relocatable tree (/opt/skiff/tools). `skiff.sh tools`
#            extracts it from the image on a host that has none of them.
#   (last)   the image: K3s, Skifflet, Plaid and the tools.
#
# Debian bookworm, for the C library: a dynamically linked program records the newest glibc
# symbol versions it was built with and does not start on an older one. Everything in the
# tools tree needs glibc 2.34 at most (checked by the build below), the glibc of RHEL 9; an
# image on Ubuntu 24.04 asked for 2.38. Both bases and the Go release are pinned by digest.
ARG GO_IMAGE=golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
ARG BASE_IMAGE=debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

# --- builder -----------------------------------------------------------------------
# Go and a C compiler come with the Go image (the compiler is for the race detector, which
# needs cgo; the Skiff binaries themselves are built with CGO_ENABLED=0). shellcheck is here
# so that the scripts are checked by the same version everywhere, and cannot be skipped. ip
# (iproute2) is for Plaid's tests that build TAPs, veth pairs and namespaces with real kernels.
FROM ${GO_IMAGE} AS builder
RUN apt-get update && \
    apt-get install -y --no-install-recommends shellcheck iproute2 && \
    rm -rf /var/lib/apt/lists/*
ENV GOPATH=/root/.go
# The image's unprivileged user (ubuntu, 1000) runs the tests: some of them (directory modes,
# user namespaces) say nothing as root. Its Go caches are in the image so that volumes mounted
# over them start with the right owner. Builds keep running as root.
RUN useradd --create-home --uid 1000 --user-group ubuntu && \
    mkdir -p /home/ubuntu/.go/pkg/mod /home/ubuntu/.cache/go-build && \
    chown -R ubuntu:ubuntu /home/ubuntu
WORKDIR /src

# --- the Skiff binaries ------------------------------------------------------------
FROM builder AS skiff-build
ARG TARGETARCH
ARG VERSION=unknown
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
# Only the source of the two modules is copied (see .dockerignore). The Go module
# and build caches live in BuildKit cache mounts, so they are reused between
# builds instead of being stored again in every layer.
COPY skifflet/ skifflet/
COPY plaid/ plaid/
RUN --mount=type=cache,target=/root/.go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOARCH=${TARGETARCH} make -C skifflet build VERSION="${VERSION}" GIT_COMMIT="${GIT_COMMIT}" BUILD_TIME="${BUILD_TIME}" && \
    CGO_ENABLED=0 GOARCH=${TARGETARCH} make -C plaid build VERSION="${VERSION}" GIT_COMMIT="${GIT_COMMIT}" BUILD_TIME="${BUILD_TIME}"

# --- the tools -----------------------------------------------------------------------
# One relocatable tree: bin/ (apptainer, slirp4netns, socat, kubectl), libexec/apptainer/
# (its starter and the helpers it runs), lib/ (the shared libraries they need beyond the C
# library, found through an RPATH relative to each program). Nothing in it is installed
# outside /opt/skiff/tools, so it can be copied to any directory (~/.skiff/tools).
FROM builder AS tools
ARG TARGETARCH
ARG APPTAINER_VERSION=1.5.4
ARG APPTAINER_SHA=ec1f51e696ba384f90bc7ab2c7a438fee70da98763d001b8f3175d14a91cfcf6
ARG SLIRP4NETNS_VERSION=v1.2.1
ARG KUBECTL_VERSION=v1.36.5
ARG SOCAT_VERSION=1.8.1.3
ARG SOCAT_SHA=06602ffd591e98c75b3dc1d66f0f19136cc666b0b2d95caad987d6ab2cb28097
ENV TOOLS=/opt/skiff/tools
RUN apt-get update && \
    apt-get install -y --no-install-recommends libseccomp-dev pkg-config patchelf patch \
        autoconf automake libtool zlib1g-dev liblzo2-dev liblzma-dev libzstd-dev liblz4-dev \
        libfuse3-dev libtalloc-dev libprotobuf-c-dev && \
    rm -rf /var/lib/apt/lists/*

# Apptainer, from the release source: no package exists for bookworm or for arm64. Not setuid:
# the bubbles are rootless. The network plugins are not built (Plaid provides the network;
# containers run with --network=none).
#
# The programs it runs (mksquashfs and unsquashfs, squashfuse_ll, fuse2fs, fuse-overlayfs and
# proot) are built from source at the versions the project's own packages bundle, with its
# own scripts/compile-dependencies, and not taken from Debian: Debian's older squashfs-tools
# and proot crash together (SIGSEGV of mksquashfs under proot) when a user converts an image
# to a SIF, which is what proot is there for (it keeps the owners of the files in the image).
RUN --mount=type=cache,target=/root/.go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -e; \
    curl -fsSLo /tmp/apptainer.tgz "https://github.com/apptainer/apptainer/releases/download/v${APPTAINER_VERSION}/apptainer-${APPTAINER_VERSION}.tar.gz" && \
    echo "${APPTAINER_SHA}  /tmp/apptainer.tgz" | sha256sum -c - && \
    tar -C /tmp -xzf /tmp/apptainer.tgz && \
    cd /tmp/apptainer-${APPTAINER_VERSION} && \
    ./mconfig --without-suid --without-network --prefix="${TOOLS}" && \
    make -C builddir -j"$(nproc)" && make -C builddir install && \
    mkdir -p /tmp/deps /tmp/deps-build && cd /tmp/deps && \
    fetch() { curl -fsSLo "$1" "$2" && echo "$3  $1" | sha256sum -c -; } && \
    fetch squashfs-tools-4.7.5.tar.gz https://github.com/plougher/squashfs-tools/archive/4.7.5/squashfs-tools-4.7.5.tar.gz 547b7b7f4d2e44bf91b6fc554664850c69563701deab9fd9cd7e21f694c88ea6 && \
    fetch squashfuse-0.6.2.tar.gz https://github.com/vasi/squashfuse/archive/0.6.2/squashfuse-0.6.2.tar.gz ae194df463ac5a9cbb25f94021d44bfb14dc21a0fc577e16348eca2fdc77c83b && \
    fetch e2fsprogs-1.47.4.tar.gz https://github.com/tytso/e2fsprogs/archive/refs/tags/v1.47.4.tar.gz 9f82eaa7002673291629077b80ee005cadfcd49854907a22007fed70b0ef596e && \
    fetch fuse-overlayfs-1.16.tar.gz https://github.com/containers/fuse-overlayfs/archive/v1.16/fuse-overlayfs-1.16.tar.gz 45968517603389ead067222d234bc8d8ed33e4b4f8ba16216bdd3e6aedcccea9 && \
    fetch PRoot-5.4.0-rootless.3.tar.gz https://github.com/rootless-containers/PRoot/archive/v5.4.0-rootless.3/PRoot-5.4.0-rootless.3.tar.gz a4399d07d5bcc27cdcbe1437e9dc9df141fbb349e151cb450e300e15eb15b1be && \
    cd /tmp/deps-build && /tmp/apptainer-${APPTAINER_VERSION}/scripts/compile-dependencies /tmp/deps >/tmp/deps.log 2>&1 || { tail -40 /tmp/deps.log; exit 1; }; \
    B="${TOOLS}/libexec/apptainer/bin"; \
    install -m 0755 squashfs-tools-4.7.5/squashfs-tools/mksquashfs squashfs-tools-4.7.5/squashfs-tools/unsquashfs "$B/"; \
    install -m 0755 squashfuse-0.6.2/squashfuse_ll e2fsprogs-1.47.4/fuse2fs fuse-overlayfs-1.16/fuse-overlayfs PRoot-5.4.0-rootless.3/src/proot "$B/"; \
    rm -rf /tmp/apptainer* /tmp/deps /tmp/deps-build

# slirp4netns: the static release binary.
RUN case "${TARGETARCH}" in \
        amd64) A=x86_64; SUM=cb1d7b7f9bc946c6179efdb8e069d5ee673c3f135668da6bf54f2ed689981833 ;; \
        arm64) A=aarch64; SUM=da4d301ffe36a0edf1cc769059c1f89eadd1afeba285636bcb2f250a6cffffed ;; \
        *) echo "Unsupported architecture: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    curl -fsSLo "${TOOLS}/bin/slirp4netns" "https://github.com/rootless-containers/slirp4netns/releases/download/${SLIRP4NETNS_VERSION}/slirp4netns-${A}" && \
    echo "${SUM}  ${TOOLS}/bin/slirp4netns" | sha256sum -c - && chmod 0755 "${TOOLS}/bin/slirp4netns"

# kubectl: the release binary, of the same Kubernetes as the cluster's.
RUN case "${TARGETARCH}" in \
        amd64) SUM=33bc88a24c3b09cf55bfd59e6ca977881b61fc3f25ab65bc80c65b430248648e ;; \
        arm64) SUM=88fc1aca8fd0c1b44fe379f67c39bd6858bdfc50241dc55775f84d0f5b31da3a ;; \
        *) echo "Unsupported architecture: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    curl -fsSLo "${TOOLS}/bin/kubectl" "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" && \
    echo "${SUM}  ${TOOLS}/bin/kubectl" | sha256sum -c - && chmod 0755 "${TOOLS}/bin/kubectl"

# socat, without OpenSSL, readline, tcp wrappers and the rest: it needs only the C library.
# (The Debian package pulls in Kerberos, OpenSSL and an RPC library.)
RUN curl -fsSLo /tmp/socat.tgz "http://www.dest-unreach.org/socat/download/socat-${SOCAT_VERSION}.tar.gz" && \
    echo "${SOCAT_SHA}  /tmp/socat.tgz" | sha256sum -c - && \
    tar -C /tmp -xzf /tmp/socat.tgz && cd /tmp/socat-${SOCAT_VERSION} && \
    ./configure --disable-openssl --disable-readline --disable-libwrap --disable-fips >/dev/null && \
    make -j"$(nproc)" >/dev/null && install -m 0755 socat "${TOOLS}/bin/socat" && \
    rm -rf /tmp/socat*

# Make the tree self-contained: strip what was built here (Apptainer keeps its symbol tables and
# debug information: its two large programs, apptainer and starter, are half the size without
# them; the stack traces of a Go program do not depend on them), copy the shared libraries the
# programs need beyond the C library, point each program at them (DT_RPATH, which also covers
# the libraries' own dependencies), and refuse a program that needs a newer C library than
# RHEL 9 has. slirp4netns and kubectl are release binaries, already stripped.
RUN set -e; mkdir -p "${TOOLS}/lib"; \
    progs="${TOOLS}/bin/apptainer ${TOOLS}/bin/socat ${TOOLS}/libexec/apptainer/bin/*"; \
    for f in $progs; do strip --strip-unneeded "$f"; done; \
    for f in $progs; do \
        ldd "$f" 2>/dev/null | awk '/=> \//{print $3}' | \
        grep -vE '/(libc|libm|libdl|libpthread|librt|libresolv|libutil|libnsl|ld-linux[^/]*)\.so' | \
        while read -r lib; do cp -Ln "$lib" "${TOOLS}/lib/"; done || true; \
    done; \
    for f in $progs; do \
        case "$f" in "${TOOLS}/bin/"*) rp='$ORIGIN/../lib' ;; *) rp='$ORIGIN/../../../lib' ;; esac; \
        patchelf --force-rpath --set-rpath "$rp" "$f" 2>/dev/null || true; \
    done; \
    for lib in "${TOOLS}"/lib/*; do patchelf --force-rpath --set-rpath '$ORIGIN' "$lib" 2>/dev/null || true; done; \
    max=$(for f in $progs "${TOOLS}"/lib/* "${TOOLS}/bin/slirp4netns"; do grep -aoE 'GLIBC_[0-9]+\.[0-9]+(\.[0-9]+)?' "$f" || true; done | sort -uV | tail -1); \
    echo "the newest C library symbol version in the tools: $max"; \
    case "$max" in GLIBC_2.34|GLIBC_2.33|GLIBC_2.32|GLIBC_2.3[01]|GLIBC_2.2*|"") ;; *) echo "ERROR: the tools need $max; RHEL 9 has 2.34" >&2; exit 1 ;; esac; \
    "${TOOLS}/bin/apptainer" --version && "${TOOLS}/bin/socat" -V | head -2 && "${TOOLS}/bin/slirp4netns" --version | head -1 && "${TOOLS}/bin/kubectl" version --client

# --- the image -----------------------------------------------------------------------
FROM ${BASE_IMAGE}
WORKDIR /

# What the bubble itself runs: curl (the API check in entrypoint.sh), ip (iproute2),
# pgrep (procps: the supervisor looks for entrypoint.sh in the instance),
# newuidmap (uidmap) and openssl (with ca-certificates: the kubelet certificates). No netfilter
# tools: Plaid needs none (the kernel of a node may not even have the modules). The outer
# slirp4netns runs on the HPC host (from the tools). drill (ldnsutils) answers the DNS
# readiness check in entrypoint.sh; dig would pull in libxml2 and libicu (about 45 MB). No
# ping and no nscd: nothing in the bubble uses them (the tests that ping run in the fixture image).
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl ldnsutils iproute2 procps uidmap && \
    rm -rf /var/lib/apt/lists/*

# The tools, where the image itself uses them from, too.
COPY --from=tools /opt/skiff/tools /opt/skiff/tools
RUN for t in apptainer slirp4netns socat kubectl; do ln -s /opt/skiff/tools/bin/$t /usr/local/bin/$t; done

# CNI plugins (host-local IPAM plugin used by Plaid)
ARG TARGETARCH
RUN if [ "${TARGETARCH}" = "amd64" ]; then \
        CNI_SHA="58c03705426e929658f45a851df15a86d06ef680cacbf3f2dc127731ca265c28"; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
        CNI_SHA="2596ef56329dd1269026f46b8df262f09ba43c92dbfb940e1e69fbccccd30a29"; \
    else \
        echo "Unsupported architecture: ${TARGETARCH}" && exit 1; \
    fi && \
    curl -LO https://github.com/containernetworking/plugins/releases/download/v1.9.0/cni-plugins-linux-${TARGETARCH}-v1.9.0.tgz && \
    echo "${CNI_SHA}  cni-plugins-linux-${TARGETARCH}-v1.9.0.tgz" | sha256sum -c - && \
    mkdir -p /opt/cni/bin && \
    (cd /opt/cni/bin && tar -zxvf /cni-plugins-linux-${TARGETARCH}-v1.9.0.tgz ./host-local) && \
    rm /cni-plugins-linux-${TARGETARCH}-v1.9.0.tgz

# K3s
ARG K3S_VERSION=v1.36.5+k3s1
RUN if [ "${TARGETARCH}" = "amd64" ]; then \
        K3S_ARCH=""; \
        K3S_SHA="d73847bcd3c5fccef0115b372e2f9a91f3032dc84bbf71518a4617565294d313"; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
        K3S_ARCH="-${TARGETARCH}"; \
        K3S_SHA="135e34cb9e8a1cfae3cb55577789e93501efe7edc080a8b2630458a85b07721e"; \
    else \
        echo "Unsupported architecture: ${TARGETARCH}" && exit 1; \
    fi && \
    curl -sfL "https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}/k3s${K3S_ARCH}" -o /usr/local/bin/k3s && \
    echo "${K3S_SHA}  /usr/local/bin/k3s" | sha256sum -c - && \
    chmod +x /usr/local/bin/k3s
RUN mkdir -p /var/lib/rancher/k3s /etc/rancher/k3s

COPY --from=skiff-build /src/skifflet/bin/skifflet /usr/bin/skifflet
COPY --from=skiff-build /src/plaid/bin/plaidd /usr/local/bin/plaidd
COPY --from=skiff-build /src/plaid/bin/plaid /usr/local/bin/plaid
COPY --from=skiff-build /src/plaid/bin/plaidctl /usr/local/bin/plaidctl
COPY --from=skiff-build /src/plaid/bin/plaidtainer /usr/local/bin/plaidtainer
RUN ln -s /usr/local/bin/plaidtainer /usr/bin/plaidtainer && \
    ln -s /usr/bin/skifflet /usr/local/bin/skifflet && \
    mkdir -p /opt/cni/bin && ln -s /usr/local/bin/plaid /opt/cni/bin/plaid

COPY images/skiff/storage.yaml /usr/share/skiff/storage.yaml
COPY images/skiff/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
