const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const tokensHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'tokens.html'), 'utf8');
const tokensCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');
const tokensScript = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');

function tokenRowTemplate() {
  const match = tokensHtml.match(/<template id="tpl-token-row">[\s\S]*?<\/template>/);
  assert.ok(match, '缺少 tpl-token-row 模板');
  return match[0];
}

test('tokens 操作列使用图标按钮而不是文字按钮', () => {
  const template = tokenRowTemplate();

  assert.match(template, /class="btn-copy-token btn-icon token-row-action-btn"/);
  assert.match(template, /class="btn-icon btn-edit token-row-action-btn"/);
  assert.match(template, /class="btn-icon btn-danger btn-delete token-row-action-btn"/);
  assert.match(template, /data-i18n-title="common\.copy" title="复制" aria-label="复制"/);
  assert.match(template, /data-i18n-title="common\.edit" title="编辑" aria-label="编辑"/);
  assert.match(template, /data-i18n-title="common\.delete" title="删除" aria-label="删除"/);
  assert.match(template, /<button[^>]*class="btn-copy-token[\s\S]*?<svg[\s\S]*?aria-hidden="true"[\s\S]*?<\/button>/);
  assert.match(template, /<button[^>]*class="btn-icon btn-edit[\s\S]*?<svg[\s\S]*?aria-hidden="true"[\s\S]*?<\/button>/);
  assert.match(template, /<button[^>]*class="btn-icon btn-danger btn-delete[\s\S]*?<svg[\s\S]*?aria-hidden="true"[\s\S]*?<\/button>/);

  assert.doesNotMatch(template, /<button[^>]*data-i18n="common\.(?:copy|edit|delete)"[^>]*>/);
  assert.doesNotMatch(template, />\s*(?:复制|编辑|删除)\s*<\/button>/);
});

test('tokens 图标按钮保持固定尺寸并支持图标内部点击', () => {
  assert.match(tokensCss, /\.token-row-action-btn\s*\{[\s\S]*?display:\s*inline-flex;[\s\S]*?width:\s*28px;[\s\S]*?height:\s*28px;[\s\S]*?padding:\s*0;/);
  assert.match(tokensCss, /\.token-row-action-btn\.btn-danger\s*\{[\s\S]*?color:\s*var\(--error-600\);/);
  assert.match(tokensScript, /const target = e\.target\.closest\('\.btn-copy-token, \.btn-edit, \.btn-delete'\);/);
});
