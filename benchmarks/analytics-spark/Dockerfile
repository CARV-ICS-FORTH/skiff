FROM maven:3.9.11-eclipse-temurin-17@sha256:e4a7ace3dc0d645ed97f8d9ad0b0d3f0b14fa8d150138f27f116d7105a639b82 AS tpcds-builder

WORKDIR /build
COPY app/pom.xml ./pom.xml
COPY app/src ./src
RUN mvn --batch-mode --no-transfer-progress -DskipTests package \
    && mvn --batch-mode --no-transfer-progress dependency:copy \
       -Dartifact=org.apache.hadoop:hadoop-aws:3.4.1 \
       -DoutputDirectory=/build/runtime-jars \
    && mvn --batch-mode --no-transfer-progress dependency:copy \
       -Dartifact=software.amazon.awssdk:bundle:2.24.6 \
       -DoutputDirectory=/build/runtime-jars

FROM golang:1.24.8-bookworm AS minio-builder

ARG MINIO_COMMIT=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a
ARG MINIO_VERSION=2025-10-15T17-29-55Z
RUN git clone --filter=blob:none https://github.com/minio/minio.git /src/minio \
    && cd /src/minio \
    && git checkout "${MINIO_COMMIT}" \
    && ldflags=$(MINIO_RELEASE=RELEASE go run buildscripts/gen-ldflags.go "${MINIO_VERSION}") \
    && CGO_ENABLED=0 go build -tags kqueue -trimpath -ldflags "$ldflags" \
       -o /usr/local/bin/minio .

FROM debian:bookworm-slim AS tpcds-kit-builder

ARG TPCDS_KIT_COMMIT=1b7fb7529edae091684201fab142d956d6afd881
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       bison build-essential byacc ca-certificates flex git \
    && git clone --filter=blob:none https://github.com/databricks/tpcds-kit.git /opt/tpcds-kit \
    && cd /opt/tpcds-kit \
    && git checkout "${TPCDS_KIT_COMMIT}" \
    && make -C tools OS=LINUX dsdgen \
    && strip tools/dsdgen

FROM spark:4.0.4-scala2.13-java17-python3-ubuntu@sha256:04ba045641e3f10b7a68981d7be35ad93c4bb9fc715a51899d7ef455140de4d2 AS cpu

USER root

ARG MC_RELEASE=RELEASE.2025-08-13T08-35-41Z
ARG MC_SHA256=01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891

RUN set -eux; \
    curl -fsSL "https://github.com/minio/mc/releases/download/${MC_RELEASE}/mc.linux-amd64.${MC_RELEASE}" -o /usr/local/bin/mc; \
    echo "${MC_SHA256}  /usr/local/bin/mc" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/mc

COPY --from=minio-builder /usr/local/bin/minio /usr/local/bin/minio
COPY --from=tpcds-kit-builder /opt/tpcds-kit /opt/tpcds-kit
COPY --from=tpcds-builder /build/target/spark-knot-tpcds.jar /opt/spark/work-dir/spark-knot-tpcds.jar
COPY --from=tpcds-builder /build/runtime-jars/*.jar /opt/spark/jars/
COPY --chown=185:185 benchmark.py runtime.py gpu-discovery.sh /opt/spark/work-dir/
COPY --chown=185:185 skiff/executor-pod-template.yaml /opt/spark/work-dir/executor-pod-template.yaml
COPY --chown=185:185 queries /opt/spark/work-dir/tpcds-queries

RUN chmod 0755 /usr/local/bin/minio /opt/spark/work-dir/gpu-discovery.sh \
    && chmod 0444 /opt/spark/work-dir/benchmark.py \
       /opt/spark/work-dir/executor-pod-template.yaml \
       /opt/spark/work-dir/spark-knot-tpcds.jar \
       /opt/spark/work-dir/tpcds-queries/*.sql \
    && chmod 0755 /opt/tpcds-kit/tools/dsdgen

USER 185

FROM tpcds-builder AS gpu-dependencies
ARG RAPIDS_VERSION=26.08.1
RUN mvn --batch-mode --no-transfer-progress dependency:copy \
    -Dartifact=com.nvidia:rapids-4-spark_2.13:${RAPIDS_VERSION} \
    -DoutputDirectory=/build/gpu-jars

FROM cpu AS gpu
USER root
ARG RAPIDS_VERSION=26.08.1
COPY --from=gpu-dependencies /build/gpu-jars/*.jar /opt/spark/jars/
RUN printf '%s\n' "$RAPIDS_VERSION" > /opt/spark/work-dir/rapids-version.txt
USER 185
