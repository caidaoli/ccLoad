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
  'www.home.meta.description': 'ccLoad 是一个高性能 AI API 网关，支持 Claude Code、Codex、Gemini、OpenAI 兼容客户端，提供智能路由、自动故障切换、模型感知冷却、协议转换和成本控制。',
  'www.home.hero.title': 'ccLoad',
  'www.home.hero.subtitle': 'Claude Code、Codex、Gemini、OpenAI 多协议 AI API 网关',
  'www.home.hero.description': '智能路由 · 自动故障切换 · 模型感知冷却 · 协议转换 · 成本控制',
  'www.home.hero.getStarted': '快速开始',
  'www.home.hero.viewGithub': 'GitHub',

  // 首页 - 核心特性
  'www.home.features.title': '核心特性',
  'www.home.features.routing.title': '智能路由',
  'www.home.features.routing.desc': '高优先级渠道先选；同优先级按有效 Key 数做平滑加权轮询。可选健康度与首字延迟惩罚。',
  'www.home.features.failover.title': '自动故障切换',
  'www.home.features.failover.desc': '按 Key / 模型 / 渠道 / URL 作用域隔离故障；401/403 只冷却当前 Key，模型级错误不误伤整个渠道。',
  'www.home.features.monitoring.title': '实时监控',
  'www.home.features.monitoring.desc': '最近 7 天服务健康度、客户端协议统计、成本分析、趋势图表和实时请求监控。',
  'www.home.features.cost.title': '成本控制',
  'www.home.features.cost.desc': '渠道每日限额、令牌总/日/月限额，OAuth 额度窗口对齐上游重置时间，精确到微美元。',
  'www.home.features.oauth.title': 'OAuth 渠道',
  'www.home.features.oauth.desc': 'Codex、Anthropic、Antigravity、xAI、Z.ai Coding Plan 与 Cursor 凭证导入；支持批量额度刷新，永久拒绝时自动禁用。',
  'www.home.features.websocket.title': 'Responses WebSocket',
  'www.home.features.websocket.desc': 'Codex 客户端保持一条下游 WebSocket，由 ccLoad 在原生 WebSocket 与 HTTP/SSE 上游之间桥接，并按完整会话故障切换。',
  'www.home.features.token.title': '本地 Token 计算',
  'www.home.features.token.desc': '<5ms 响应，93%+ 准确度，无需调用上游即可估算 Token。',
  'www.home.features.protocol.title': '自动协议回退',
  'www.home.features.protocol.desc': '先试客户端协议，再按 OpenAI、Anthropic、Codex、Gemini 探测，且不重复已试协议。',
  'www.home.features.thinking.title': '思考后缀',
  'www.home.features.thinking.desc': '模型名加 (high)/(xhigh)/(16384) 即可跨协议写入思考参数；路由、冷却和上游模型名始终用基名。',
  'www.home.features.detection.title': '软错误检测',
  'www.home.features.detection.desc': 'HTTP 200 伪装的错误也能检测，SSE 流式响应中的限流标记按 429 处理。',
  'www.home.features.proxy.title': '渠道级代理与时段',
  'www.home.features.proxy.desc': '单个渠道可独立走 HTTP/HTTPS/SOCKS5 代理，并设置 HH:MM 可用时段；时段外完全不参与路由。',
  'www.home.features.keys.title': 'Key 模型白名单',
  'www.home.features.keys.desc': '每个 Key 可限定服务哪些渠道模型；全部 Key 都不匹配则跳过该渠道，不冷却、不记失败。',

  // 首页 - OAuth 提供商
  'www.home.oauth.section.title': '第一方账号渠道',
  'www.home.oauth.section.desc': '在管理后台导入凭证，不必把上游 Key 发给客户端。OAuth 授权与刷新仍走官方端点；可在系统设置覆盖数据面地址。',
  'www.home.oauth.codex.title': 'Codex / ChatGPT',
  'www.home.oauth.codex.desc': 'OAuth 或个人访问令牌（PAT）。PAT 以 at- 开头，导入时校验账号；刷新被永久拒绝时禁用渠道。',
  'www.home.oauth.anthropic.title': 'Anthropic / Claude',
  'www.home.oauth.anthropic.desc': 'Claude OAuth 凭证自动刷新，额度窗口与标准成本对齐上游配额。',
  'www.home.oauth.antigravity.title': 'Antigravity',
  'www.home.oauth.antigravity.desc': 'Google Cloud Code 渠道，daily 与 daily sandbox 固定回退；容量耗尽先冷却当前模型。',
  'www.home.oauth.xai.title': 'xAI',
  'www.home.oauth.xai.desc': 'Grok OAuth 渠道。grok-4.6 及之后的对话模型可通过 Images API 走 image_generation 工具。',
  'www.home.oauth.zai.title': 'Z.ai Coding Plan',
  'www.home.oauth.zai.desc': '浏览器授权或直接导入 Coding Plan API Key；模型目录按账号实时拉取，额度不记标准成本窗口。',
  'www.home.oauth.cursor.title': 'Cursor',
  'www.home.oauth.cursor.desc': '导入 User API Key。需要时自动安装官方 SDK Bridge，不在网关本机执行 Cursor 内建工具。',

  // 首页 - 管理后台预览
  'www.home.admin.title': '不是黑盒代理',
  'www.home.admin.desc': 'ccLoad 把渠道、OAuth 凭证、模型、令牌、成本、首字延迟、失败原因和模型验证放到同一个后台里。管理员负责配置网关；API Token 用户只能只读查看获准渠道和自身用量数据。',
  'www.home.admin.item1': '管理员可查看全局请求、Token、成本和延迟；API Token 会话只显示自身作用域。',
  'www.home.admin.item2': '既能对话式测试模型，也能通过真实代理链路按渠道测试；支持思考等级、内置搜索和图片生成。',
  'www.home.admin.item3': '渠道支持多 URL、多 Key、OAuth 导入、单模型停用、Key 模型白名单，以及 RPM、并发、时段和每日成本限额。',
  'www.home.admin.item4': '导出对话为 Markdown / HTML 后，可结合调试日志查看脱敏后的上游请求与响应，定位协议转换问题。',
  'www.home.admin.usage': '查看使用指南',
  'www.home.admin.config': '查看配置手册',
  'www.home.admin.imageAlt': 'ccLoad 管理后台统计界面截图',
  'www.home.admin.caption': '管理后台把运行状态和成本口径暴露出来，不靠猜。',

  // 首页 - 部署方式
  'www.home.deployment.title': '部署方式',
  'www.home.deployment.docker.title': 'Docker 部署',
  'www.home.deployment.docker.difficulty': '难度：⭐⭐',
  'www.home.deployment.docker.desc': '推荐生产环境。官方镜像提供 latest、beta 和精确版本标签，支持 SQLite、MySQL 和 PostgreSQL',
  'www.home.deployment.docker.learnMore': '查看 Docker 步骤',
  'www.home.deployment.hf.title': 'Hugging Face',
  'www.home.deployment.hf.difficulty': '难度：⭐',
  'www.home.deployment.hf.desc': '免费托管，自动 HTTPS，开箱即用，2 CPU + 16GB RAM',
  'www.home.deployment.hf.learnMore': '查看 Spaces 步骤',
  'www.home.deployment.source.title': '源码编译',
  'www.home.deployment.source.difficulty': '难度：⭐⭐⭐',
  'www.home.deployment.source.desc': '适合开发者，支持定制构建，需要 Go 1.26+',
  'www.home.deployment.source.learnMore': '查看源码编译',
  'www.home.deployment.binary.title': '二进制下载',
  'www.home.deployment.binary.difficulty': '难度：⭐⭐',
  'www.home.deployment.binary.desc': '从 GitHub Releases 下载即用，支持 Linux / macOS / Windows',
  'www.home.deployment.binary.learnMore': '查看二进制运行',

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
  'www.config.meta.description': 'ccLoad 环境变量、存储模式、渠道路由、OAuth 地址、令牌限额、成本控制和运行时配置说明。',
  'www.config.subtitle': '先配置启动安全项，再通过管理后台热更新渠道、限额和超时策略',

  // 使用页
  'www.usage.title': 'API 使用',
  'www.usage.meta.description': 'ccLoad Anthropic、OpenAI、Gemini、Codex 兼容 API 使用示例，包含 Responses WebSocket、思考后缀与 Images API。',
  'www.usage.subtitle': 'ccLoad 暴露标准 Anthropic、OpenAI、Gemini 与 Codex 兼容端点，并支持 Responses WebSocket、思考后缀与 Images API；客户端只需要换 base URL 和访问令牌',

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
