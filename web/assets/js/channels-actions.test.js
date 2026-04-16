const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');

test('渠道卡片模板包含复制操作按钮', () => {
  const templateMatch = html.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.match(template, /class="btn-icon channel-action-btn"\s+data-action="copy"/);
  assert.match(template, /data-channel-id="\{\{id\}\}"/);
  assert.match(template, /data-channel-name="\{\{name\}\}"/);
});

test('渠道卡片模板保留上游协议徽章并为额外协议标签预留插槽', () => {
  const templateMatch = html.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.match(template, /\{\{\{typeBadge\}\}\}<strong>\{\{name\}\}<\/strong>\{\{\{protocolTransformBadges\}\}\}/);
  assert.doesNotMatch(template, /\(ID:\s*\{\{id\}\}\)/);
});

test('渠道卡片模板不再把禁用徽章挂在标题行，而是由优先级列承载', () => {
  const templateMatch = html.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.doesNotMatch(template, /\{\{\{disabledBadge\}\}\}<\/div>\s*<div class="ch-name-statuses">/);
  assert.match(template, /<td class="ch-col-priority"[^>]*>\s*\{\{\{effectivePriorityHtml\}\}\}\s*<\/td>/);
});

test('操作列为五个操作按钮保留足够宽度', () => {
  const actionsColumnStyle = css.match(/\.ch-col-actions\s*\{[^}]+\}/);
  assert.ok(actionsColumnStyle, '缺少 .ch-col-actions 样式');

  const styleBlock = actionsColumnStyle[0];
  assert.match(styleBlock, /width:\s*220px/);
  assert.match(styleBlock, /min-width:\s*220px/);
  assert.match(styleBlock, /max-width:\s*220px/);
});

test('排序卡片模板保留 data-channel-id 但不再显示渠道 ID 文案', () => {
  const templateMatch = html.match(/<template id="tpl-sort-item">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-sort-item 模板');

  const template = templateMatch[0];
  assert.match(template, /class="sort-item" data-channel-id="\{\{id\}\}"/);
  assert.doesNotMatch(template, /\(ID:\s*\{\{id\}\}\)/);
  assert.doesNotMatch(template, /sort-item-id/);
});
