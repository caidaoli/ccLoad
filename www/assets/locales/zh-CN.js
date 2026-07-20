/**
 * ccLoad 介绍网站中文语言包
 */
window.I18N_LOCALES = window.I18N_LOCALES || {};
window.I18N_LOCALES['zh-CN'] = Object.assign(window.I18N_LOCALES['zh-CN'] || {}, {
  // 导航
  'www.nav.home': '产品概览',
  'www.nav.install': '部署安装',
  'www.nav.config': '配置手册',
  'www.nav.usage': 'API 使用',
  'www.nav.feedback': '反馈支持',
  'www.nav.github': 'GitHub',
  'www.nav.switchLanguage': '切换语言',
  'www.nav.switchTheme': '切换主题',

  // 首页 - Hero
  'www.home.meta.title': 'ccLoad - Claude Code、Codex、Gemini、OpenAI 多协议 AI API 网关',
  'www.home.meta.description': 'ccLoad 是一个高性能 AI API 网关，支持 Claude Code、Codex、Gemini、OpenAI 兼容客户端，提供智能路由、自动故障切换、实时监控和成本控制。',
  'www.home.hero.title': 'ccLoad',
  'www.home.hero.subtitle': 'Claude Code & Codex & Gemini & OpenAI 兼容 API 代理服务',
  'www.home.hero.description': '智能路由 · 自动故障切换 · 实时监控 · 成本控制',
  'www.home.hero.getStarted': '快速开始',
  'www.home.hero.viewGithub': 'GitHub',

  // 首页 - 核心特性
  'www.home.features.title': '核心特性',
  'www.home.features.routing.title': '智能路由',
  'www.home.features.routing.desc': '按优先级、成功率和可选的首字相对延迟动态排序，同优先级内使用平滑加权轮询均衡流量',
  'www.home.features.failover.title': '自动故障切换',
  'www.home.features.failover.desc': '优先隔离故障 Key 和模型，只有可用资源全部冷却后才升级为渠道冷却',
  'www.home.features.monitoring.title': '实时监控',
  'www.home.features.monitoring.desc': '详细的请求统计、成本分析、趋势图表，实时请求监控大屏',
  'www.home.features.cost.title': '成本控制',
  'www.home.features.cost.desc': '渠道每日成本限额、API 令牌费用限额，精确到微美元',
  'www.home.features.multiapi.title': '多 API 兼容',
  'www.home.features.multiapi.desc': 'Claude/Codex/Gemini/OpenAI 四大协议完全兼容，一套配置走天下',
  'www.home.features.token.title': '本地 Token 计算',
  'www.home.features.token.desc': '<5ms 响应，93%+ 准确度，无需调用 API 即可计算 Token 数量',
  'www.home.features.protocol.title': '协议转换',
  'www.home.features.protocol.desc': 'Anthropic/OpenAI/Gemini/Codex 互转，保留采样、思考与内置搜索意图',
  'www.home.features.detection.title': '软错误检测',
  'www.home.features.detection.desc': 'HTTP 200 伪装的错误也能检测，SSE 流式响应中的限流标记识别',
  'www.home.features.proxy.title': '渠道级代理',
  'www.home.features.proxy.desc': '单个渠道可独立使用 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理，不污染全局代理策略',
  'www.home.features.dns.title': 'DNS 主机覆盖',
  'www.home.features.dns.desc': '上游 DNS 故障时可将域名钉到固定 IP，同时保留 TLS SNI、证书校验和 Host 头',
  'www.home.features.quota.title': '配额感知冷却',
  'www.home.features.quota.desc': '识别供应商固定窗口配额响应，按真实 reset 时间冷却渠道，而不是盲猜退避时间',
  'www.home.features.autoupdate.title': '自动更新',
  'www.home.features.autoupdate.desc': '默认每 12 小时检查新版本，可在管理后台设置页调整自动更新间隔',

  // 首页 - 管理后台预览
  'www.home.admin.title': '不是黑盒代理',
  'www.home.admin.desc': 'ccLoad 把渠道、模型、令牌、成本、首字延迟、失败原因和对话式模型测试放到同一个后台里。管理员负责配置网关；API Token 用户只能只读查看获准渠道和自身用量数据。',
  'www.home.admin.item1': '管理员可查看全局请求、Token、成本和延迟；API Token 会话只显示自身作用域。',
  'www.home.admin.item2': '对话式模型测试支持图片上传、思考等级、模型内置搜索和流式控制。',
  'www.home.admin.item3': '渠道支持多 URL、多 Key、RPM 限制、并发限制和每日成本限额。',
  'www.home.admin.item4': '导出对话为 Markdown / HTML 后，可结合调试日志查看脱敏后的上游请求与响应，定位协议转换问题。',
  'www.home.admin.usage': '查看使用指南',
  'www.home.admin.config': '查看配置手册',
  'www.home.admin.imageAlt': 'ccLoad 管理后台统计界面截图',
  'www.home.admin.caption': '管理后台把运行状态和成本口径暴露出来，不靠猜。',

  // 首页 - 部署方式
  'www.home.deployment.title': '部署方式',
  'www.home.deployment.docker.title': 'Docker 部署',
  'www.home.deployment.docker.difficulty': '难度：⭐⭐',
  'www.home.deployment.docker.desc': '推荐生产环境使用，稳定可靠，支持 SQLite、MySQL 和 PostgreSQL',
  'www.home.deployment.docker.learnMore': '查看详情',
  'www.home.deployment.hf.title': 'Hugging Face',
  'www.home.deployment.hf.difficulty': '难度：⭐',
  'www.home.deployment.hf.desc': '免费托管，自动 HTTPS，开箱即用，2 CPU + 16GB RAM',
  'www.home.deployment.hf.learnMore': '查看详情',
  'www.home.deployment.source.title': '源码编译',
  'www.home.deployment.source.difficulty': '难度：⭐⭐⭐',
  'www.home.deployment.source.desc': '适合开发者，支持魔改，需要 Go 1.25+ 环境',
  'www.home.deployment.source.learnMore': '查看详情',
  'www.home.deployment.binary.title': '二进制下载',
  'www.home.deployment.binary.difficulty': '难度：⭐⭐',
  'www.home.deployment.binary.desc': '懒人福音，下载即用，支持多平台（Linux/macOS/Windows）',
  'www.home.deployment.binary.learnMore': '查看详情',

  // 首页 - 快速开始
  'www.home.quickstart.title': '快速开始',
  'www.home.quickstart.docker': 'Docker',
  'www.home.quickstart.hf': 'Hugging Face',
  'www.home.quickstart.source': '源码编译',
  'www.home.quickstart.binary': '二进制',

  // 安装页
  'www.install.title': '部署安装',
  'www.install.meta.description': 'ccLoad Docker、Hugging Face Spaces、源码编译和二进制部署指南，覆盖安全启动项、存储和 API 令牌配置。',
  'www.install.subtitle': '从本地试用到生产部署，按场景选择最少配置路径',

  // 配置页
  'www.config.title': '配置手册',
  'www.config.meta.description': 'ccLoad 环境变量、存储模式、渠道路由、令牌限额、成本控制和运行时配置说明。',
  'www.config.subtitle': '先配置启动安全项，再通过管理后台热更新渠道、限额和超时策略',

  // 使用页
  'www.usage.title': 'API 使用',
  'www.usage.meta.description': 'ccLoad Anthropic、OpenAI、Gemini、Codex 兼容 API 使用示例，包含原生 Codex Alpha Search 透传。',
  'www.usage.subtitle': 'ccLoad 暴露标准 Anthropic、OpenAI、Gemini 与 Codex 兼容端点，并支持原生 Codex Alpha Search 透传；客户端只需要换 base URL 和访问令牌',

  // 反馈页
  'www.feedback.title': '反馈支持',
  'www.feedback.meta.description': 'ccLoad Bug 反馈、功能建议、使用讨论、Pull Request 和安全问题提交入口。',
  'www.feedback.subtitle': 'Bug、功能建议、使用讨论和安全问题分别走清晰渠道，别把问题埋在聊天记录里',

  // 通用
  'www.common.copy': '复制',
  'www.common.copied': '已复制！',
  'www.common.learnMore': '了解更多',
  'www.common.getStarted': '开始使用',
});
