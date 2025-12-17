# ccLoad Docker镜像构建文件
# syntax=docker/dockerfile:1.4

# 构建阶段
FROM golang:1.25.0-alpine AS builder

# 版本号参数（优先使用 --build-arg，否则尝试从 git 获取）
ARG VERSION
ARG COMMIT

# 安装构建依赖
RUN apk add --no-cache git ca-certificates tzdata gcc musl-dev

# 设置工作目录
WORKDIR /app

# 设置Go模块代理
ENV GOPROXY=https://proxy.golang.org,direct

# 复制go mod文件
COPY go.mod go.sum ./

# 下载依赖
RUN --mount=type=cache,target=/root/.cache/go-mod \
    go mod download

# 复制源代码
COPY . .

# 编译二进制文件（注入版本信息用于静态资源缓存控制）
# VERSION 为空时从 git tag 获取，都没有则默认 "dev"
ENV CGO_ENABLED=1
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    BUILD_VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo "dev")} && \
    BUILD_COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")} && \
    BUILD_COMMIT=$(echo $BUILD_COMMIT | cut -c1-7) && \
    BUILD_TIME=$(date '+%Y-%m-%d %H:%M:%S %z') && \
    go build \
    -tags go_json \
    -ldflags="-s -w \
      -X ccLoad/internal/version.Version=${BUILD_VERSION} \
      -X ccLoad/internal/version.Commit=${BUILD_COMMIT} \
      -X 'ccLoad/internal/version.BuildTime=${BUILD_TIME}' \
      -X ccLoad/internal/version.BuiltBy=docker" \
    -o ccload .

# 运行阶段
FROM alpine:latest

# 安装运行时依赖
RUN apk --no-cache add ca-certificates tzdata

# 创建非root用户
RUN addgroup -g 1001 -S ccload && \
    adduser -u 1001 -S ccload -G ccload

# 设置工作目录
WORKDIR /app

# 从构建阶段复制二进制文件
COPY --from=builder /app/ccload .

# 复制Web静态文件
COPY --from=builder /app/web ./web

# 创建数据目录并设置权限
RUN mkdir -p /app/data && \
    chown -R ccload:ccload /app

# 切换到非root用户
USER ccload

# 暴露端口
EXPOSE 8080

# 设置环境变量
ENV PORT=8080
ENV SQLITE_PATH=/app/data/ccload.db
ENV GIN_MODE=release

# 健康检查（轻量级端点，<5ms响应）
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# 启动应用
CMD ["./ccload"]
