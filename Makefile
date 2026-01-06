# ccLoad Makefile - macOS Service Management

# 变量定义
SERVICE_NAME = com.ccload.service
PLIST_TEMPLATE = $(SERVICE_NAME).plist.template
PLIST_FILE = $(SERVICE_NAME).plist
LAUNCH_AGENTS_DIR = $(HOME)/Library/LaunchAgents
TARGET_PLIST = $(LAUNCH_AGENTS_DIR)/$(PLIST_FILE)
BINARY_NAME = ccload
LOG_DIR = logs
PROJECT_DIR = $(shell pwd)
GOTAGS ?= go_json

# 版本信息
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date '+%Y-%m-%d %H:%M:%S %z')
BUILT_BY ?= $(shell whoami)
VERSION_PKG = ccLoad/internal/version
LDFLAGS = -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X '$(VERSION_PKG).BuildTime=$(BUILD_TIME)' \
	-X $(VERSION_PKG).BuiltBy=$(BUILT_BY)

.PHONY: help build docker-build generate-plist inject-env-vars install-service uninstall-service start stop restart status logs clean

# 默认目标
help:
	@echo "ccLoad 服务管理 Makefile"
	@echo ""
	@echo "可用命令:"
	@echo "  build             - 构建二进制文件"
	@echo "  docker-build      - 构建 Docker 镜像（自动注入版本信息）"
	@echo "  generate-plist    - 从模板生成 plist 文件（自动读取 .env 配置）"
	@echo "  install-service   - 安装 LaunchAgent 服务"
	@echo "  uninstall-service - 卸载 LaunchAgent 服务"
	@echo "  start            - 启动服务"
	@echo "  stop             - 停止服务"
	@echo "  restart          - 重启服务"
	@echo "  status           - 查看服务状态"
	@echo "  logs             - 查看服务日志"
	@echo "  clean            - 清理构建文件和日志"

# 构建二进制文件（纯Go静态编译 + trimpath）
build:
	@echo "构建 $(BINARY_NAME) ($(VERSION))..."
	@CGO_ENABLED=0 go build -tags "$(GOTAGS)" -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY_NAME) .
	@echo "构建完成: $(BINARY_NAME)"

# 构建 Docker 镜像（自动注入版本信息）
DOCKER_IMAGE ?= ccload
DOCKER_TAG ?= $(VERSION)
docker-build:
	@echo "构建 Docker 镜像 $(DOCKER_IMAGE):$(DOCKER_TAG)..."
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) \
		-t $(DOCKER_IMAGE):latest \
		.
	@echo "Docker 镜像构建完成: $(DOCKER_IMAGE):$(DOCKER_TAG)"

# 创建必要的目录

# 生成 plist 文件（从模板动态替换路径和环境变量）
generate-plist:
	@echo "从模板生成 plist 文件..."
	@# 首先进行基础路径替换
	@sed 's|{{PROJECT_DIR}}|$(PROJECT_DIR)|g' $(PLIST_TEMPLATE) > $(PLIST_FILE).tmp
	@# 如果存在 .env 文件，则注入环境变量
	@if [ -f ".env" ]; then \
		echo "检测到 .env 文件，注入环境变量..."; \
		$(MAKE) inject-env-vars; \
	else \
		echo "未找到 .env 文件，使用默认环境变量"; \
		mv $(PLIST_FILE).tmp $(PLIST_FILE); \
	fi
	@echo "plist 文件已生成: $(PLIST_FILE)"

# 注入 .env 文件中的环境变量到 plist 文件
inject-env-vars:
	@# 创建环境变量临时文件
	@echo "" > .env_vars.tmp
	@# 解析 .env 文件
	@grep -v '^[[:space:]]*#' .env | grep -v '^[[:space:]]*$$' | while IFS='=' read -r key value; do \
		if [ -n "$$key" ]; then \
			key=$$(echo "$$key" | sed 's/^[[:space:]]*//;s/[[:space:]]*$$//'); \
			value=$$(echo "$$value" | sed 's/^[[:space:]]*//;s/[[:space:]]*$$//' | sed 's/^["'\'']\(.*\)["'\'']$$/\1/'); \
			value=$$(echo "$$value" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g; s/'\''/\&#39;/g'); \
			echo "        <key>$$key</key>" >> .env_vars.tmp; \
			echo "        <string>$$value</string>" >> .env_vars.tmp; \
		fi; \
	done
	@# 在 PATH 后插入环境变量
	@awk '/<string>\/usr\/local\/bin:\/usr\/bin:\/bin<\/string>/{print; system("cat .env_vars.tmp"); next}1' $(PLIST_FILE).tmp > $(PLIST_FILE)
	@# 清理临时文件
	@rm -f $(PLIST_FILE).tmp .env_vars.tmp

# 安装服务
install-service: build generate-plist
	@echo "安装 LaunchAgent 服务..."
	@mkdir -p $(LOG_DIR)
	@mkdir -p $(LAUNCH_AGENTS_DIR)
	@if [ -f "$(TARGET_PLIST)" ]; then \
		echo "服务已存在，先卸载旧服务..."; \
		$(MAKE) uninstall-service; \
	fi
	@cp $(PLIST_FILE) $(TARGET_PLIST)
	@launchctl load $(TARGET_PLIST)
	@echo "服务安装完成并已启动"
	@$(MAKE) status

# 卸载服务
uninstall-service:
	@echo "卸载 LaunchAgent 服务..."
	@if [ -f "$(TARGET_PLIST)" ]; then \
		launchctl unload $(TARGET_PLIST) 2>/dev/null || true; \
		rm -f $(TARGET_PLIST); \
		echo "服务已卸载"; \
	else \
		echo "服务未安装"; \
	fi

# 启动服务
start:
	@echo "启动服务..."
	@launchctl start $(SERVICE_NAME)
	@sleep 1
	@$(MAKE) status

# 停止服务
stop:
	@echo "停止服务..."
	@launchctl stop $(SERVICE_NAME)
	@sleep 1
	@$(MAKE) status

# 重启服务
restart: stop start

# 查看服务状态
status:
	@echo "服务状态:"
	@launchctl list | grep $(SERVICE_NAME) || echo "服务未运行"

# 查看日志
logs:
	@echo "=== 标准输出日志 ==="
	@if [ -f "$(LOG_DIR)/ccload.log" ]; then \
		tail -f $(LOG_DIR)/ccload.log; \
	else \
		echo "日志文件不存在: $(LOG_DIR)/ccload.log"; \
	fi

# 查看错误日志
error-logs:
	@echo "=== 错误日志 ==="
	@if [ -f "$(LOG_DIR)/ccload.error.log" ]; then \
		tail -f $(LOG_DIR)/ccload.error.log; \
	else \
		echo "错误日志文件不存在: $(LOG_DIR)/ccload.error.log"; \
	fi

# 清理文件
clean:
	@echo "清理构建文件和日志..."
	@rm -f $(BINARY_NAME)
	@rm -f $(PLIST_FILE)
	@rm -rf $(LOG_DIR)
	@echo "清理完成"

# 开发模式运行（不作为服务）
dev:
	@echo "开发模式运行..."
	@go run -tags "$(GOTAGS)" .

# 查看完整服务信息
info:
	@echo "=== 服务信息 ==="
	@echo "服务名称: $(SERVICE_NAME)"
	@echo "配置文件: $(PLIST_FILE)"
	@echo "安装路径: $(TARGET_PLIST)"
	@echo "二进制文件: $(BINARY_NAME)"
	@echo "日志目录: $(LOG_DIR)"
	@echo ""
	@$(MAKE) status
