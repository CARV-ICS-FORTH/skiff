FROM alpine:3.20.3 AS fixture
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

# The same image with a USER, named, as an image of a non-root service has: the user and the
# group of its passwd entry are what a container runs as when its pod does not say.
FROM fixture AS fixture-user
RUN addgroup -g 4322 appgroup && adduser -D -u 4321 -G appgroup appuser
USER appuser
