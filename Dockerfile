# ccLoad Docker镜像构建文件
# 基于 Alpine Linux 的多阶段构建，优化镜像大小和安全性

# 构建阶段
FROM golang:1.24.0-alpine AS builder

# 设置工作目录
WORKDIR /app

# 安装构建依赖
RUN apk add --no-cache git ca-certificates tzdata gcc musl-dev

# 复制 go mod 文件
COPY go.mod go.sum ./

# 下载依赖
RUN go mod download

# 复制源代码
COPY . .

# 构建二进制文件（移除-a标志避免Sonic库兼容性问题）
RUN CGO_ENABLED=1 GOOS=linux go build -tags go_json -ldflags="-s -w" -o ccload .

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

# 健康检查
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/public/summary || exit 1

# 启动应用
CMD ["./ccload"]