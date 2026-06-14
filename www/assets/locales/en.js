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
  'www.home.features.protocol.desc': 'Transform between Anthropic/OpenAI/Gemini/Codex, preserving sampling and thinking params',
  'www.home.features.detection.title': 'Soft Error Detection',
  'www.home.features.detection.desc': 'Detect errors disguised as HTTP 200, identify rate-limit markers in SSE streams',

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
  'www.install.subtitle': 'Pick the smallest deployment path that fits your runtime, from local testing to production.',

  // Config
  'www.config.title': 'Configuration Guide',
  'www.config.subtitle': 'Set the secure startup options first, then tune channels, limits, and timeout policy from the admin console.',

  // Usage
  'www.usage.title': 'API Usage',
  'www.usage.subtitle': 'ccLoad exposes Anthropic, OpenAI, Gemini, and Codex-compatible endpoints. Clients only need a new base URL and token.',

  // Feedback
  'www.feedback.title': 'Support',
  'www.feedback.subtitle': 'Use the right channel for bugs, feature requests, usage discussions, and security reports.',

  // Common
  'www.common.copy': 'Copy',
  'www.common.copied': 'Copied!',
  'www.common.learnMore': 'Learn More',
  'www.common.getStarted': 'Get Started',
});
