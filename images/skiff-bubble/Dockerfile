# Builder stage
ARG REGISTRY=docker.io/chazapis
ARG BASE_IMAGE=ubuntu:24.04
FROM ${REGISTRY}/skiff-builder:latest AS builder
ARG TARGETARCH
ARG VERSION=unknown
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
WORKDIR /src
# Only the source of the two modules is copied (see .dockerignore). The Go module
# and build caches live in BuildKit cache mounts, so they are reused between
# builds instead of being stored again in every layer.
COPY skifflet/ skifflet/
COPY plaid/ plaid/
RUN --mount=type=cache,target=/root/.go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOARCH=${TARGETARCH} make -C skifflet build VERSION="${VERSION}" GIT_COMMIT="${GIT_COMMIT}" BUILD_TIME="${BUILD_TIME}" && \
    CGO_ENABLED=0 GOARCH=${TARGETARCH} make -C plaid build VERSION="${VERSION}" GIT_COMMIT="${GIT_COMMIT}" BUILD_TIME="${BUILD_TIME}"

FROM ${BASE_IMAGE}
WORKDIR /

# Basic utilities, iptables, and nscd. Outer slirp4netns runs on the VM/HPC host.
# drill (ldnsutils) answers the DNS readiness check in entrypoint.sh; dig would
# pull in libxml2 and libicu (about 45 MB).
RUN apt-get update && \
    apt-get install -y --no-install-recommends curl ldnsutils iproute2 iputils-ping iptables nscd && \
    rm -rf /var/lib/apt/lists/*

# Apptainer
RUN apt-get update && \
    apt-get install -y --no-install-recommends gpg ca-certificates && \
    mkdir -p /etc/apt/keyrings && \
    curl -fsSL "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0xAFE36534FC6218AE" | gpg --dearmor -o /etc/apt/keyrings/apptainer.gpg && \
    echo "deb [signed-by=/etc/apt/keyrings/apptainer.gpg] https://ppa.launchpadcontent.net/apptainer/ppa/ubuntu noble main" > /etc/apt/sources.list.d/apptainer-ubuntu-ppa-noble.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends apptainer && \
    rm -rf /var/lib/apt/lists/* && \
    # Plaid provides the network (containers run with --network=none) and nothing
    # here uses encrypted images, so Apptainer's bundled CNI plugins and gocryptfs are dead weight.
    rm -rf /usr/libexec/apptainer/cni /usr/libexec/apptainer/bin/gocryptfs

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
ARG K3S_VERSION=v1.35.0+k3s1
RUN if [ "${TARGETARCH}" = "amd64" ]; then \
        K3S_ARCH=""; \
        K3S_SHA="959c9310a6ab893958d1c95bc5d7609de9d7884630c8832180f059369b6dc331"; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
        K3S_ARCH="-${TARGETARCH}"; \
        K3S_SHA="c0b1673a1f2b740cb7bb08355c885efb6a4aa5c8022e5bb306c621c8c1492883"; \
    else \
        echo "Unsupported architecture: ${TARGETARCH}" && exit 1; \
    fi && \
    curl -sfL "https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}/k3s${K3S_ARCH}" -o /usr/local/bin/k3s && \
    echo "${K3S_SHA}  /usr/local/bin/k3s" | sha256sum -c - && \
    chmod +x /usr/local/bin/k3s
RUN mkdir -p /var/lib/rancher/k3s /etc/rancher/k3s

# kube-proxy
ARG KUBE_VERSION=v1.35.0
RUN if [ "${TARGETARCH}" = "amd64" ]; then \
        KUBE_SHA="6489ece90b0400cc275204601126d797f86b4b3642672227460280ee659c8f54"; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
        KUBE_SHA="d7254f50c4aea1963c534dbdbae58ac16087667ebb129bfa781bcdd9f7920972"; \
    else \
        echo "Unsupported architecture: ${TARGETARCH}" && exit 1; \
    fi && \
    curl -L "https://dl.k8s.io/release/${KUBE_VERSION}/bin/linux/${TARGETARCH}/kube-proxy" -o /usr/local/bin/kube-proxy && \
    echo "${KUBE_SHA}  /usr/local/bin/kube-proxy" | sha256sum -c - && \
    chmod +x /usr/local/bin/kube-proxy

COPY --from=builder /src/skifflet/bin/skifflet /usr/bin/skifflet
COPY --from=builder /src/plaid/bin/plaidd /usr/local/bin/plaidd
COPY --from=builder /src/plaid/bin/plaid /usr/local/bin/plaid
COPY --from=builder /src/plaid/bin/plaidctl /usr/local/bin/plaidctl
COPY --from=builder /src/plaid/bin/plaidtainer /usr/local/bin/plaidtainer
RUN ln -s /usr/local/bin/plaidtainer /usr/bin/plaidtainer && \
    ln -s /usr/bin/skifflet /usr/local/bin/skifflet && \
    mkdir -p /opt/cni/bin && ln -s /usr/local/bin/plaid /opt/cni/bin/plaid

COPY images/skiff-bubble/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
