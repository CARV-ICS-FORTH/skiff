FROM ubuntu:24.04

# Install Go 1.26 from PPA
RUN apt-get update && \
    apt-get install -y --no-install-recommends software-properties-common make && \
    add-apt-repository ppa:longsleep/golang-backports && \
    apt-get update && \
    apt-get install -y --no-install-recommends golang-1.26 && \
    rm -rf /var/lib/apt/lists/*

# Set Go environment variables
ENV GOROOT=/usr/lib/go-1.26
ENV GOPATH=/root/.go
ENV PATH=$PATH:$GOROOT/bin:$GOPATH/bin

WORKDIR /src

