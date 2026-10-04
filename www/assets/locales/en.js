/**
 * ccLoad Website English Locale
 * 页面正文的英文原文直接写在 HTML 里；这里只放构建期生成的导航、页脚等片段文案。
 */
window.I18N_LOCALES = window.I18N_LOCALES || {};
window.I18N_LOCALES['en'] = Object.assign(window.I18N_LOCALES['en'] || {}, {
  // Navigation
  'www.nav.label': 'Main',
  'www.nav.home': 'Overview',
  'www.nav.install': 'Deploy',
  'www.nav.config': 'Configure',
  'www.nav.usage': 'API Usage',
  'www.nav.feedback': 'Support',
  'www.nav.switchLanguage': '切换到中文',
  'www.nav.switchTheme': 'Switch theme',
  'www.nav.toggleMenu': 'Toggle menu',

  // Footer
  'www.footer.tagline': 'Open-source, self-hosted AI API gateway for Claude Code, Codex, Gemini and OpenAI-compatible clients.',
  'www.footer.docs': 'Documentation',
  'www.footer.project': 'Project',
  'www.footer.source': 'Source code',
  'www.footer.releases': 'Releases',
  'www.footer.image': 'Docker image',
  'www.footer.issues': 'Issues'
});
