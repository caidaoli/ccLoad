# ccLoad Makefile - macOS Service Management

# 变量定义
SERVICE_NAME = com.ccload.service
PLIST_FILE = $(SERVICE_NAME).plist
LAUNCH_AGENTS_DIR = $(HOME)/Library/LaunchAgents
TARGET_PLIST = $(LAUNCH_AGENTS_DIR)/$(PLIST_FILE)
BINARY_NAME = ccload
LOG_DIR = logs
GOTAGS ?= go_json

.PHONY: help build install-service uninstall-service start stop restart status logs clean

# 默认目标
help:
	@echo "ccLoad 服务管理 Makefile"
	@echo ""
	@echo "可用命令:"
	@echo "  build             - 构建二进制文件"
	@echo "  install-service   - 安装 LaunchAgent 服务"
	@echo "  uninstall-service - 卸载 LaunchAgent 服务"
	@echo "  start            - 启动服务"
	@echo "  stop             - 停止服务"
	@echo "  restart          - 重启服务"
	@echo "  status           - 查看服务状态"
	@echo "  logs             - 查看服务日志"
	@echo "  clean            - 清理构建文件和日志"

# 构建二进制文件
build:
	@echo "构建 $(BINARY_NAME)..."
	@go build -tags "$(GOTAGS)" -o $(BINARY_NAME) .
	@echo "构建完成: $(BINARY_NAME)"

# 创建必要的目录

# 安装服务
install-service: build
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
