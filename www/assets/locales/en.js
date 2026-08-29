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
  'www.home.meta.description': 'ccLoad is a high-performance AI API gateway for Claude Code, Codex, Gemini and OpenAI-compatible clients with smart routing, automatic failover, model-aware cooldown, protocol transforms and cost control.',
  'www.home.hero.title': 'ccLoad',
  'www.home.hero.subtitle': 'AI API gateway for Claude Code, Codex, Gemini, and OpenAI',
  'www.home.hero.description': 'Smart routing · Automatic failover · Model-aware cooldown · Protocol transforms · Cost control',
  'www.home.hero.getStarted': 'Get Started',
  'www.home.hero.viewGithub': 'GitHub',

  // Home - Features
  'www.home.features.title': 'Core Features',
  'www.home.features.routing.title': 'Smart Routing',
  'www.home.features.routing.desc': 'Higher-priority channels first; equal priority uses smooth weighted round-robin by usable keys. Optional health and first-byte scoring.',
  'www.home.features.failover.title': 'Auto Failover',
  'www.home.features.failover.desc': 'Isolate failures at key, model, channel, or URL scope. 401/403 cool only the current key; model-scoped errors do not disable the whole channel.',
  'www.home.features.monitoring.title': 'Real-time Monitoring',
  'www.home.features.monitoring.desc': 'Seven-day service health, client-protocol statistics, cost analysis, trends, and live request monitoring.',
  'www.home.features.cost.title': 'Cost Control',
  'www.home.features.cost.desc': 'Daily channel limits, token total/daily/monthly limits, and OAuth quota windows aligned to upstream reset times, tracked in micro-dollars.',
  'www.home.features.oauth.title': 'OAuth Channels',
  'www.home.features.oauth.desc': 'Import Codex, Anthropic, Antigravity, xAI, Z.ai Coding Plan, Cursor, and Zed credentials. Batch-refresh quotas; permanently rejected credentials disable the channel.',
  'www.home.features.websocket.title': 'Responses WebSocket',
  'www.home.features.websocket.desc': 'Keep one Codex client WebSocket while ccLoad bridges native WebSocket and HTTP/SSE upstreams with transcript-aware failover.',
  'www.home.features.token.title': 'Local Token Count',
  'www.home.features.token.desc': '<5ms response, 93%+ accuracy, count tokens without calling upstream.',
  'www.home.features.protocol.title': 'Automatic Protocol Fallback',
  'www.home.features.protocol.desc': 'Try the client protocol first, then probe OpenAI, Anthropic, Codex, and Gemini without retrying it.',
  'www.home.features.thinking.title': 'Thinking Suffix',
  'www.home.features.thinking.desc': 'Append (high)/(xhigh)/(16384) to a model name to set thinking parameters across protocols. Routing, cooldown, and the upstream name always use the base name.',
  'www.home.features.detection.title': 'Soft Error Detection',
  'www.home.features.detection.desc': 'Detect errors disguised as HTTP 200. Explicit rate-limit markers in SSE streams are handled as 429.',
  'www.home.features.proxy.title': 'Per-channel Proxy and Windows',
  'www.home.features.proxy.desc': 'Route a channel through HTTP, HTTPS, or SOCKS5, and optionally restrict it to an HH:MM availability window. Outside the window it is excluded from routing.',
  'www.home.features.keys.title': 'Per-Key Model Allowlists',
  'www.home.features.keys.desc': 'Restrict which channel models each key serves. If every key declines the model, the channel is skipped with no cooldown and no failure recorded.',

  // Home - OAuth providers
  'www.home.oauth.section.title': 'First-party account channels',
  'www.home.oauth.section.desc': 'Import credentials in the admin console. Clients never hold upstream keys. OAuth authorization and refresh still use official endpoints; optional system settings override data-plane addresses.',
  'www.home.oauth.codex.title': 'Codex / ChatGPT',
  'www.home.oauth.codex.desc': 'OAuth or a personal access token (PAT). PATs start with at- and are verified on import. A permanently rejected refresh disables the channel.',
  'www.home.oauth.anthropic.title': 'Anthropic / Claude',
  'www.home.oauth.anthropic.desc': 'Claude OAuth credentials refresh automatically. Quota windows and standard-cost tracking follow the upstream reset schedule.',
  'www.home.oauth.antigravity.title': 'Antigravity',
  'www.home.oauth.antigravity.desc': 'Google Cloud Code channels with a fixed daily → daily sandbox fallback. Capacity exhaustion cools the current model first.',
  'www.home.oauth.xai.title': 'xAI',
  'www.home.oauth.xai.desc': 'Grok OAuth channels. Conversational models at grok-4.6 and later can use the Images API through the image_generation tool.',
  'www.home.oauth.zai.title': 'Z.ai Coding Plan',
  'www.home.oauth.zai.desc': 'Browser authorization or a Coding Plan API key. The model catalog is loaded from the account; quota windows are not billed as standard cost.',
  'www.home.oauth.cursor.title': 'Cursor',
  'www.home.oauth.cursor.desc': 'Import a Cursor user API key. ccLoad installs the official SDK Bridge when needed and does not run Cursor built-in tools on the gateway host.',
  'www.home.oauth.zed.title': 'Zed',
  'www.home.oauth.zed.desc': 'Native app sign-in over an ephemeral RSA keypair; the callback token is decrypted into a long-lived credential. Trial access binds the real Zed installation system ID. A fixed /completions upstream runs Codex-protocol local transforms.',

  // Home - Admin preview
  'www.home.admin.title': 'Not a black-box proxy',
  'www.home.admin.desc': 'ccLoad puts channels, OAuth credentials, models, tokens, cost, first-byte latency, failure reasons, and model verification into one console. Administrators manage the gateway; API-token users get a read-only view scoped to their permitted channels and usage data.',
  'www.home.admin.item1': 'Track requests, tokens, cost and latency system-wide as an administrator or within one API token\'s scope.',
  'www.home.admin.item2': 'Run chat-style tests or test models by channel through the real proxy path, including reasoning level, built-in search, and image generation.',
  'www.home.admin.item3': 'Configure multiple URLs and keys, import OAuth credentials, pause individual models, set per-key model allowlists, and enforce RPM, concurrency, time windows, and daily cost limits.',
  'www.home.admin.item4': 'Export conversations as Markdown or HTML, then inspect masked upstream requests and responses when tracking protocol transform issues.',
  'www.home.admin.usage': 'View API usage',
  'www.home.admin.config': 'View configuration',
  'www.home.admin.imageAlt': 'ccLoad admin dashboard statistics screenshot',
  'www.home.admin.caption': 'The admin console exposes runtime state and cost accounting, so you do not have to guess.',

  // Home - Deployment
  'www.home.deployment.title': 'Deployment Options',
  'www.home.deployment.docker.title': 'Docker',
  'www.home.deployment.docker.difficulty': 'Difficulty: ⭐⭐',
  'www.home.deployment.docker.desc': 'Recommended for production. Official images use latest, beta, and exact version tags. Supports SQLite, MySQL, and PostgreSQL.',
  'www.home.deployment.docker.learnMore': 'View Docker steps',
  'www.home.deployment.hf.title': 'Hugging Face',
  'www.home.deployment.hf.difficulty': 'Difficulty: ⭐',
  'www.home.deployment.hf.desc': 'Free hosting, automatic HTTPS, ready to run, 2 CPU + 16GB RAM',
  'www.home.deployment.hf.learnMore': 'View Spaces steps',
  'www.home.deployment.source.title': 'From Source',
  'www.home.deployment.source.difficulty': 'Difficulty: ⭐⭐⭐',
  'www.home.deployment.source.desc': 'For developers and custom builds, requires Go 1.26+',
  'www.home.deployment.source.learnMore': 'View source build',
  'www.home.deployment.binary.title': 'Binary',
  'www.home.deployment.binary.difficulty': 'Difficulty: ⭐⭐',
  'www.home.deployment.binary.desc': 'Download from GitHub Releases and run. Builds for Linux, macOS, and Windows.',
  'www.home.deployment.binary.learnMore': 'View binary runtime',

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
  'www.config.meta.description': 'Configure ccLoad environment variables, storage modes, channel routing, OAuth addresses, token limits, cost controls and runtime settings.',
  'www.config.subtitle': 'Set the secure startup options first, then tune channels, limits, and timeout policy from the admin console.',

  // Usage
  'www.usage.title': 'API Usage',
  'www.usage.meta.description': 'Use ccLoad with Anthropic, OpenAI, Gemini and Codex-compatible API endpoints, including Responses WebSocket, thinking suffixes, and the Images API.',
  'www.usage.subtitle': 'ccLoad exposes Anthropic, OpenAI, Gemini, and Codex-compatible endpoints, including Responses WebSocket, thinking suffixes, and the Images API. Clients only need a new base URL and token.',

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
