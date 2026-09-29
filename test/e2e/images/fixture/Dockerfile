FROM alpine:3.20.3
RUN apk add --no-cache \
    curl \
    wget \
    busybox-extras \
    bind-tools \
    openssl \
    coreutils \
    procps \
    bash \
    python3
COPY fixture.sh /usr/local/bin/fixture
COPY app.py /usr/local/lib/fixture/app.py
RUN chmod +x /usr/local/bin/fixture
ENTRYPOINT ["/usr/local/bin/fixture"]
