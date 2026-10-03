# syntax=docker/dockerfile:1

# 多阶段构建：前端(node) → 后端(golang) → 运行时(alpine)
# 最终产物为静态二进制（CGO_ENABLED=0），运行时镜像无需 glibc/musl 兼容处理；
# SQLite 使用纯 Go 驱动（modernc.org/sqlite），因此不需要 cgo。
#
# 分层说明：先构建依赖（package.json / go.mod）以利用缓存，再复制源码。

# ---- 前端构建阶段 -----------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /web
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
# 构建产物由 vite.config.ts 的 outDir 直接写到 /internal/web/dist
RUN npm run build

# ---- 后端编译阶段 -----------------------------------------------------------
FROM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src

# 先拷依赖清单以利用层缓存：依赖不变时跳过 go mod download
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
# 用构建阶段产出的前端资源覆盖占位资源
COPY --from=web /internal/web/dist/ internal/web/dist/

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
    -ldflags "-s -w -X main.buildVersion=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/zen-gateway ./cmd/gateway

# ---- 运行阶段 ---------------------------------------------------------------
FROM alpine:3.21

# ca-certificates：上游与 GitHub API 均为 https
# tzdata：zen 按天冷却到次日 0 点，需要正确时区
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S zen \
    && adduser -S -G zen -h /nonexistent -s /sbin/nologin zen \
    && mkdir -p /data && chown zen:zen /data

COPY --from=builder /out/zen-gateway /usr/local/bin/zen-gateway

# 非 root 运行
USER zen

# SQLite 数据库与统计数据持久化在此卷
VOLUME ["/data"]

EXPOSE 8080

# 说明：
#   ZEN_LISTEN      监听地址（默认 :8080）
#   ZEN_ADMIN_TOKEN Web 管理端口令；未设置则不启用 /admin
#   ZEN_DATA_DIR    数据目录（默认 /data）
#   ZEN_DB_PATH     SQLite 文件路径
#   ZEN_RETENTION_DAYS 统计保留天数（默认 30）
#
# 上游 Key、代理、接入 Token 等均在 Web 管理端维护（存入 SQLite）。
ENV ZEN_LISTEN=":8080" \
    ZEN_DATA_DIR="/data"

ENTRYPOINT ["/usr/local/bin/zen-gateway"]

# healthcheck 复用网关自身的 /healthz 端点
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${ZEN_LISTEN##*:}/healthz" >/dev/null 2>&1 || exit 1