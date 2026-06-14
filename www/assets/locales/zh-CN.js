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
  'www.home.hero.title': 'ccLoad',
  'www.home.hero.subtitle': 'Claude Code & Codex & Gemini & OpenAI 兼容 API 代理服务',
  'www.home.hero.description': '智能路由 · 自动故障切换 · 实时监控 · 成本控制',
  'www.home.hero.getStarted': '快速开始',
  'www.home.hero.viewGithub': 'GitHub',

  // 首页 - 核心特性
  'www.home.features.title': '核心特性',
  'www.home.features.routing.title': '智能路由',
  'www.home.features.routing.desc': '基于优先级和健康度的智能请求分发，平滑加权轮询确保流量均衡',
  'www.home.features.failover.title': '自动故障切换',
  'www.home.features.failover.desc': '渠道故障时秒级切换，指数退避冷却机制避免雪崩',
  'www.home.features.monitoring.title': '实时监控',
  'www.home.features.monitoring.desc': '详细的请求统计、成本分析、趋势图表，实时请求监控大屏',
  'www.home.features.cost.title': '成本控制',
  'www.home.features.cost.desc': '渠道每日成本限额、API 令牌费用限额，精确到微美元',
  'www.home.features.multiapi.title': '多 API 兼容',
  'www.home.features.multiapi.desc': 'Claude/Codex/Gemini/OpenAI 四大协议完全兼容，一套配置走天下',
  'www.home.features.token.title': '本地 Token 计算',
  'www.home.features.token.desc': '<5ms 响应，93%+ 准确度，无需调用 API 即可计算 Token 数量',
  'www.home.features.protocol.title': '协议转换',
  'www.home.features.protocol.desc': 'Anthropic/OpenAI/Gemini/Codex 互转，保留采样与思考参数',
  'www.home.features.detection.title': '软错误检测',
  'www.home.features.detection.desc': 'HTTP 200 伪装的错误也能检测，SSE 流式响应中的限流标记识别',

  // 首页 - 部署方式
  'www.home.deployment.title': '部署方式',
  'www.home.deployment.docker.title': 'Docker 部署',
  'www.home.deployment.docker.difficulty': '难度：⭐⭐',
  'www.home.deployment.docker.desc': '推荐生产环境使用，稳定可靠，支持 SQLite 和 MySQL',
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
  'www.install.subtitle': '从本地试用到生产部署，按场景选择最少配置路径',

  // 配置页
  'www.config.title': '配置手册',
  'www.config.subtitle': '先配置启动安全项，再通过管理后台热更新渠道、限额和超时策略',

  // 使用页
  'www.usage.title': 'API 使用',
  'www.usage.subtitle': 'ccLoad 暴露标准 Anthropic、OpenAI、Gemini 与 Codex 兼容端点，客户端只需要换 base URL 和访问令牌',

  // 反馈页
  'www.feedback.title': '反馈支持',
  'www.feedback.subtitle': 'Bug、功能建议、使用讨论和安全问题分别走清晰渠道，别把问题埋在聊天记录里',

  // 通用
  'www.common.copy': '复制',
  'www.common.copied': '已复制！',
  'www.common.learnMore': '了解更多',
  'www.common.getStarted': '开始使用',
});
