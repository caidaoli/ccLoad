# ccLoad Docker镜像构建文件
# 多平台构建：使用 tonistiigi/xx 交叉编译，避免 QEMU 模拟
# syntax=docker/dockerfile:1.4

# ============================================
# 阶段1: 基础工具链 (与 TARGETPLATFORM 无关，可复用)
# ============================================
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS base

# 安装交叉编译工具链（这层很少变，缓存命中率高）
COPY --from=tonistiigi/xx:1.6.1 / /
RUN apk add --no-cache git ca-certificates tzdata clang lld

WORKDIR /app

# ============================================
# 阶段2: 依赖下载 (go.mod 不变就复用)
# ============================================
FROM base AS deps

# 设置Go模块代理
ENV GOPROXY=https://proxy.golang.org,direct

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# ============================================
# 阶段3: 构建 (仅此处依赖 TARGETPLATFORM)
# ============================================
FROM deps AS builder

# 版本号参数（带默认值，更健壮）
ARG VERSION=dev
ARG COMMIT=unknown

# 配置目标平台的交叉编译工具链
ARG TARGETPLATFORM
RUN xx-apk add musl-dev gcc

# 复制源代码
COPY . .

# 静态编译
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    BUILD_VERSION=${VERSION} && \
    BUILD_COMMIT=$(echo "${COMMIT}" | cut -c1-7) && \
    BUILD_TIME=$(date '+%Y-%m-%d %H:%M:%S %z') && \
    xx-go build \
    -tags go_json \
    -buildvcs=false \
    -trimpath \
    -ldflags="-s -w \
      -X ccLoad/internal/version.Version=${BUILD_VERSION} \
      -X ccLoad/internal/version.Commit=${BUILD_COMMIT} \
      -X 'ccLoad/internal/version.BuildTime=${BUILD_TIME}' \
      -X ccLoad/internal/version.BuiltBy=docker" \
    -o ccload . && \
    xx-verify ccload

# ============================================
# 阶段4: 运行时镜像 (最小化)
# ============================================
FROM alpine:3.21

# 安装运行时依赖
RUN apk --no-cache add ca-certificates tzdata

# 创建非root用户
RUN addgroup -g 1001 -S ccload && \
    adduser -u 1001 -S ccload -G ccload

WORKDIR /app

# 从构建阶段复制
COPY --from=builder /app/ccload .
COPY --from=builder /app/web ./web

# 创建数据目录并设置权限
RUN mkdir -p /app/data && \
    chown -R ccload:ccload /app

USER ccload

EXPOSE 8080

ENV PORT=8080 \
    SQLITE_PATH=/app/data/ccload.db \
    GIN_MODE=release

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

CMD ["./ccload"]
