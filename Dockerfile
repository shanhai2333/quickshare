# ---- 构建阶段 ----
FROM golang:1.25-alpine AS builder

WORKDIR /src
# 纯 Go 实现，无需 CGO，产物是静态二进制
ENV CGO_ENABLED=0

# 版本信息由构建方传进来（CI 传 git tag / commit / 构建时间，
# 本地 `docker build .` 不传就是 dev）。**不在镜像里硬编码版本号**：
# 硬编码的东西迟早和实际编出来的代码对不上，而用户报问题时报的就是它。
ARG VERSION=dev
ARG COMMIT=
ARG BUILD_DATE=
ARG REPO=

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath \
      -ldflags="-s -w \
        -X quickshare/internal/version.Version=${VERSION} \
        -X quickshare/internal/version.Commit=${COMMIT} \
        -X quickshare/internal/version.Date=${BUILD_DATE} \
        -X quickshare/internal/version.Repo=${REPO}" \
      -o /out/quickshare .

# ---- 运行阶段 ----
FROM alpine:3.21

# 这几个 ARG 要在每个用到它们的阶段重新声明一次（ARG 不跨阶段继承）
ARG VERSION=dev
ARG REPO=

LABEL org.opencontainers.image.title="QuickShare" \
      org.opencontainers.image.description="跑在 NAS 上的内网文件分享服务：上传下载文件、发一段文本给别的设备看" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/${REPO}" \
      org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 1000 -G users quickshare \
 && mkdir -p /data && chown quickshare:users /data

WORKDIR /app
COPY --from=builder /out/quickshare /app/quickshare

ENV QS_ADDR=":8080" \
    QS_DATA_DIR="/data" \
    TZ="Asia/Shanghai"

EXPOSE 8080
VOLUME ["/data"]

# 注意：默认以 uid 1000 运行。若 NAS 上的数据目录属主不同，
# 在 docker-compose.yml 里用 user: "${PUID}:${PGID}" 覆盖，或先 chown。
USER quickshare

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/config >/dev/null || exit 1

ENTRYPOINT ["/app/quickshare"]
