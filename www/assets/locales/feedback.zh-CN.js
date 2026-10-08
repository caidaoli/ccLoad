/**
 * ccLoad 介绍网站 feedback.html 正文中文译文
 * 英文源文即 HTML 内联内容；键对应 data-i18n-html，值可含行内标记。
 */
window.I18N_LOCALES = window.I18N_LOCALES || {};
window.I18N_LOCALES['zh-CN'] = Object.assign(window.I18N_LOCALES['zh-CN'] || {}, {
  'www.feedback.eyebrow': '支持',
  'www.feedback.choose.title': '按问题类型选择渠道',
  'www.feedback.choose.desc': '反馈渠道越对口，修复越快。没有日志、版本信息或复现步骤的报告，通常还得来回追问。',
  'www.feedback.bug.title': 'Bug 反馈',
  'www.feedback.bug.desc': '请求失败、自动协议回退异常、统计数据不对或界面行为问题，都走 Issues。',
  'www.feedback.bug.action': '提交 Issue',
  'www.feedback.feature.title': '功能建议',
  'www.feedback.feature.desc': '新协议、新提供商、新指标、部署支持或管理后台改进，都请从使用场景说起。',
  'www.feedback.feature.action': '查看 Issues',
  'www.feedback.discuss.title': '使用讨论',
  'www.feedback.discuss.desc': '部署选型、渠道配置、模型选择和成本策略，多半是讨论而不是 Bug。',
  'www.feedback.discuss.action': '打开 Discussions',
  'www.feedback.security.title': '安全问题',
  'www.feedback.security.desc': '认证绕过、敏感数据泄露、请求规则注入或 TLS 风险，不要公开贴出利用细节。',
  'www.feedback.security.action': '安全页面',
  'www.feedback.contribute.title': '贡献代码',
  'www.feedback.contribute.desc': 'Bug 修复和功能开发都要保持聚焦。大范围重构先开 Issue。',
  'www.feedback.contribute.action': '查看 PR',
  'www.feedback.links.title': '项目链接',
  'www.feedback.links.repo': 'GitHub 仓库',
  'www.feedback.links.releases': '发行版',
  'www.feedback.links.license': 'MIT 许可证',
  'www.feedback.template.title': '高信噪比的反馈模板',
  'www.feedback.template.desc': '问题越容易复现，就越容易直接修掉。这份模板覆盖了大部分部署、代理和前端问题。',
  'www.feedback.template.warning': '不要公开 API Key、访问令牌或密码。',
  'www.feedback.template.body': '## 环境\n- ccLoad 版本：\n- 部署方式：Docker / Hugging Face / 源码 / 二进制\n- 存储模式：SQLite / MySQL / PostgreSQL / 混合\n- 客户端：Claude Code / Codex / OpenAI SDK / Gemini SDK / curl\n\n## 现象\n- 请求端点：\n- 响应状态码：\n- 预期行为：\n- 实际行为：\n\n## 复现步骤\n1.\n2.\n3.\n\n## 日志\n只贴脱敏后的日志片段。不要贴 API Key、访问令牌或管理密码。',
});
