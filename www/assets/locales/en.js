/**
 * ccLoad Website English Locale
 */
window.I18N_LOCALES = window.I18N_LOCALES || {};
window.I18N_LOCALES['en'] = Object.assign(window.I18N_LOCALES['en'] || {}, {
  // Navigation
  'www.nav.home': 'Overview',
  'www.nav.install': 'Deploy',
  'www.nav.config': 'Configure',
  'www.nav.usage': 'API Usage',
  'www.nav.feedback': 'Support',
  'www.nav.github': 'GitHub',
  'www.nav.switchLanguage': 'Switch language',
  'www.nav.switchTheme': 'Switch theme',

  // Home - Hero
  'www.home.meta.title': 'ccLoad - AI API Gateway for Claude Code, Codex, Gemini and OpenAI',
  'www.home.meta.description': 'ccLoad is a high-performance AI API gateway for Claude Code, Codex, Gemini and OpenAI-compatible clients with smart routing, automatic failover, real-time monitoring and cost control.',
  'www.home.hero.title': 'ccLoad',
  'www.home.hero.subtitle': 'Claude Code & Codex & Gemini & OpenAI Compatible API Proxy',
  'www.home.hero.description': 'Smart Routing · Auto Failover · Real-time Monitoring · Cost Control',
  'www.home.hero.getStarted': 'Get Started',
  'www.home.hero.viewGithub': 'GitHub',

  // Home - Features
  'www.home.features.title': 'Core Features',
  'www.home.features.routing.title': 'Smart Routing',
  'www.home.features.routing.desc': 'Intelligent request distribution based on priority and health, smooth weighted round-robin ensures traffic balance',
  'www.home.features.failover.title': 'Auto Failover',
  'www.home.features.failover.desc': 'Failover in seconds when channel fails, exponential backoff cooldown prevents cascading failures',
  'www.home.features.monitoring.title': 'Real-time Monitoring',
  'www.home.features.monitoring.desc': 'Detailed request statistics, cost analysis, trend charts, and real-time request monitoring dashboard',
  'www.home.features.cost.title': 'Cost Control',
  'www.home.features.cost.desc': 'Daily cost limits per channel, API token cost limits, accurate to micro-dollars',
  'www.home.features.multiapi.title': 'Multi-API Support',
  'www.home.features.multiapi.desc': 'Fully compatible with Claude/Codex/Gemini/OpenAI, one config for all',
  'www.home.features.token.title': 'Local Token Count',
  'www.home.features.token.desc': '<5ms response, 93%+ accuracy, count tokens without API calls',
  'www.home.features.protocol.title': 'Protocol Transform',
  'www.home.features.protocol.desc': 'Transform between Anthropic/OpenAI/Gemini/Codex, preserving sampling, thinking and built-in search intent',
  'www.home.features.detection.title': 'Soft Error Detection',
  'www.home.features.detection.desc': 'Detect errors disguised as HTTP 200, identify rate-limit markers in SSE streams',
  'www.home.features.proxy.title': 'Per-channel Proxy',
  'www.home.features.proxy.desc': 'Route individual channels through HTTP, HTTPS, SOCKS5 or SOCKS5H proxies without changing global proxy policy',
  'www.home.features.dns.title': 'DNS Host Override',
  'www.home.features.dns.desc': 'Pin broken upstream hostnames to fixed IPs while preserving TLS SNI, certificates and Host headers',
  'www.home.features.quota.title': 'Quota-aware Cooldown',
  'www.home.features.quota.desc': 'Understand provider fixed-window quota responses and cool down the channel until the real reset time',

  // Home - Admin preview
  'www.home.admin.title': 'Not a black-box proxy',
  'www.home.admin.desc': 'ccLoad puts channels, models, tokens, cost, first-byte latency, failure reasons, and chat-style model testing into one console. When routing looks wrong, read the data; when upstreams fail, read the logs.',
  'www.home.admin.item1': 'Track requests, tokens, cost and latency by channel, model and token.',
  'www.home.admin.item2': 'Run chat-style model testing with image upload, reasoning level, built-in search and streaming controls.',
  'www.home.admin.item3': 'Configure multiple URLs, multiple keys, RPM limits, concurrency limits and daily cost limits per channel.',
  'www.home.admin.item4': 'Export conversations as Markdown or HTML, then inspect masked upstream requests and responses when tracking protocol transform issues.',
  'www.home.admin.usage': 'View API usage',
  'www.home.admin.config': 'View configuration',
  'www.home.admin.imageAlt': 'ccLoad admin dashboard statistics screenshot',
  'www.home.admin.caption': 'The admin console exposes runtime state and cost accounting, so you do not have to guess.',

  // Home - Deployment
  'www.home.deployment.title': 'Deployment Options',
  'www.home.deployment.docker.title': 'Docker',
  'www.home.deployment.docker.difficulty': 'Difficulty: ⭐⭐',
  'www.home.deployment.docker.desc': 'Recommended for production, stable and reliable, supports SQLite and MySQL',
  'www.home.deployment.docker.learnMore': 'Learn More',
  'www.home.deployment.hf.title': 'Hugging Face',
  'www.home.deployment.hf.difficulty': 'Difficulty: ⭐',
  'www.home.deployment.hf.desc': 'Free hosting, auto HTTPS, out-of-the-box, 2 CPU + 16GB RAM',
  'www.home.deployment.hf.learnMore': 'Learn More',
  'www.home.deployment.source.title': 'From Source',
  'www.home.deployment.source.difficulty': 'Difficulty: ⭐⭐⭐',
  'www.home.deployment.source.desc': 'For developers, customizable, requires Go 1.25+ environment',
  'www.home.deployment.source.learnMore': 'Learn More',
  'www.home.deployment.binary.title': 'Binary',
  'www.home.deployment.binary.difficulty': 'Difficulty: ⭐⭐',
  'www.home.deployment.binary.desc': 'Easy to use, download and run, supports multiple platforms (Linux/macOS/Windows)',
  'www.home.deployment.binary.learnMore': 'Learn More',

  // Home - Quick Start
  'www.home.quickstart.title': 'Quick Start',
  'www.home.quickstart.docker': 'Docker',
  'www.home.quickstart.hf': 'Hugging Face',
  'www.home.quickstart.source': 'From Source',
  'www.home.quickstart.binary': 'Binary',

  // Install
  'www.install.title': 'Deploy ccLoad',
  'www.install.meta.description': 'Deploy ccLoad with Docker, Hugging Face Spaces, source builds or release binaries. Configure secure startup options, storage and API tokens for production.',
  'www.install.subtitle': 'Pick the smallest deployment path that fits your runtime, from local testing to production.',

  // Config
  'www.config.title': 'Configuration Guide',
  'www.config.meta.description': 'Configure ccLoad environment variables, storage modes, channel routing, token limits, cost controls and runtime settings.',
  'www.config.subtitle': 'Set the secure startup options first, then tune channels, limits, and timeout policy from the admin console.',

  // Usage
  'www.usage.title': 'API Usage',
  'www.usage.meta.description': 'Use ccLoad with Anthropic, OpenAI, Gemini and Codex-compatible API endpoints. Includes client examples, token setup and admin console guidance.',
  'www.usage.subtitle': 'ccLoad exposes Anthropic, OpenAI, Gemini, and Codex-compatible endpoints. Clients only need a new base URL and token.',

  // Feedback
  'www.feedback.title': 'Support',
  'www.feedback.meta.description': 'Get support for ccLoad bugs, feature requests, discussions, pull requests and security reports.',
  'www.feedback.subtitle': 'Use the right channel for bugs, feature requests, usage discussions, and security reports.',

  // Common
  'www.common.copy': 'Copy',
  'www.common.copied': 'Copied!',
  'www.common.learnMore': 'Learn More',
  'www.common.getStarted': 'Get Started',
});
