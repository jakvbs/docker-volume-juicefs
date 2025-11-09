FROM golang:1.25 AS builder

ARG GOPROXY
ENV GOPROXY=${GOPROXY:-"https://proxy.golang.org,direct"}
ARG JUICEFS_CE_VERSION
ENV JUICEFS_CE_VERSION=${JUICEFS_CE_VERSION:-"1.3.0"}
ARG TARGETARCH
ARG JUICEFS_EE_URL
ARG JFSMOUNT_URL=""

WORKDIR /docker-volume-juicefs
COPY . .
RUN apt-get update && apt-get install -y curl musl-tools tar gzip && \
    CC=/usr/bin/musl-gcc go build -o bin/docker-volume-juicefs --ldflags '-linkmode external -extldflags "-static"' .

WORKDIR /workspace
RUN if [ "$TARGETARCH" = "arm64" ]; then \
        ARCH="arm64"; \
    else \
        ARCH="amd64"; \
    fi && \
    curl -fsSL -o juicefs-ce.tar.gz https://github.com/juicedata/juicefs/releases/download/v${JUICEFS_CE_VERSION}/juicefs-${JUICEFS_CE_VERSION}-linux-${ARCH}.tar.gz && \
    tar -zxf juicefs-ce.tar.gz && \
    mv juicefs /tmp/juicefs && \
    curl -fsSL -o /juicefs ${JUICEFS_EE_URL:-https://s.juicefs.com/static/juicefs} && \
    chmod +x /juicefs && \
    JFSM_URL_DEFAULT="https://s.juicefs.com/static/Linux/mount" && \
    curl -fsSL -o /bin/jfsmount "${JFSMOUNT_URL:-$JFSM_URL_DEFAULT}" && chmod +x /bin/jfsmount

FROM python:3.12-alpine
RUN mkdir -p /run/docker/plugins /jfs/state /jfs/volumes
COPY --from=builder /docker-volume-juicefs/bin/docker-volume-juicefs /
COPY --from=builder /tmp/juicefs /bin/
COPY --from=builder /juicefs /usr/bin/
COPY --from=builder /bin/jfsmount /bin/jfsmount
# Make sure juicefs CLI sees a stable mount helper and doesn't try to auto-update
ENV JFS_NO_UPDATE=1
ENV JFS_MOUNT_BIN=/bin/jfsmount
RUN /usr/bin/juicefs version && chmod +x /bin/juicefs
CMD ["docker-volume-juicefs"]
