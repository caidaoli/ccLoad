# ccLoad Docker镜像构建文件
# 使用交叉编译架构，彻底解决ARM64构建慢的问题
# 技术方案：tonistiigi/xx + Clang/LLVM交叉编译（无QEMU开销）

# 语法特性：启用BuildKit新特性
# syntax=docker/dockerfile:1.4

# 构建阶段 - 关键：使用BUILDPLATFORM在原生架构执行
FROM --platform=$BUILDPLATFORM golang:1.25.0-alpine AS builder

# 安装交叉编译工具链
# tonistiigi/xx提供跨架构编译辅助工具
COPY --from=tonistiigi/xx:1.6.1 / /
RUN apk add --no-cache git ca-certificates tzdata clang lld

# 设置工作目录
WORKDIR /app

# 设置Go模块代理
ENV GOPROXY=https://proxy.golang.org,direct

# 配置目标平台的交叉编译工具链
# ARG TARGETPLATFORM由buildx自动注入（如linux/arm64）
ARG TARGETPLATFORM
RUN xx-apk add musl-dev gcc

# 复制go mod文件
COPY go.mod go.sum ./

# 下载依赖（在原生平台执行，速度快）
RUN --mount=type=cache,target=/root/.cache/go-mod \
    go mod download

# 复制源代码
COPY . .

# 交叉编译二进制文件
# xx-go自动设置GOOS/GOARCH/CC等环境变量
# 关键优势：AMD64主机直接生成ARM64二进制，完全避免QEMU模拟
ENV CGO_ENABLED=1
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    xx-go build \
    -tags go_json \
    -ldflags="-s -w" \
    -o ccload . && \
    xx-verify ccload

# 运行阶段 - 必须指定目标平台，避免QEMU模拟导致apk失败
FROM --platform=$TARGETPLATFORM alpine:latest

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

# 健康检查
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/public/summary || exit 1

# 启动应用
CMD ["./ccload"]
