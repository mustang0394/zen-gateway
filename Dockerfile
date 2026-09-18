# syntax=docker/dockerfile:1

# 多阶段构建：golang 编译 → alpine 运行时
# 目标产物静态二进制（CGO_ENABLED=0），运行时镜像无需 glibc/musl 兼容问题。

# ---- 编译阶段 ---------------------------------------------------------------
# go.mod 声明 go 1.27；builder 镜像版本可保持略新
FROM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src

# 先拷 go.mod/go.sum 以利用层缓存：依赖不变时跳过 go mod download
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
    -ldflags "-s -w -X main.buildVersion=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/zen-gateway ./cmd/gateway

# ---- 运行阶段 ---------------------------------------------------------------
FROM alpine:3.21

# ca-certificates：上游是 https（opencode.ai / GitHub API）
# tzdata：限流按天计算，保持时区行为一致
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S zen \
    && adduser -S -G zen -h /nonexistent -s /sbin/nologin zen

COPY --from=builder /out/zen-gateway /usr/local/bin/zen-gateway

# 非 root 运行
USER zen

EXPOSE 8080

# Docker 部署不需要映射配置文件，全部通过环境变量：
#   ZEN_LISTEN  — 监听地址（默认 :8080）
#   ZEN_PROXIES — key→代理 JSON，如 {"oc_sk_x":"socks5h://u:p@host:1080"}
ENV ZEN_LISTEN=":8080" \
    ZEN_PROXIES=""

ENTRYPOINT ["/usr/local/bin/zen-gateway"]

# healthcheck 直接复用网关自身的 /healthz 端点
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${ZEN_LISTEN##*:}/healthz" >/dev/null 2>&1 || exit 1
