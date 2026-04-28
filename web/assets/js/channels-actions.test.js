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

test('渠道卡片模板不再渲染已禁用文字徽章', () => {
  const templateMatch = html.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.doesNotMatch(template, /disabledBadge/);
  assert.match(template, /<td class="ch-col-priority"[^>]*>\s*\{\{\{effectivePriorityHtml\}\}\}\s*<\/td>/);
  assert.doesNotMatch(template, /channels\.statusDisabled/);
});

test('渠道卡片模板把启用开关放在独立列，操作列不再包含 toggle 按钮', () => {
  const templateMatch = html.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.match(template, /<td class="ch-col-enabled"[^>]*>[\s\S]*class="channel-enable-switch channel-action-btn \{\{toggleSwitchClass\}\}"[\s\S]*role="switch"[\s\S]*aria-checked="\{\{enabled\}\}"/);

  const actionsMatch = template.match(/<td class="ch-col-actions"[\s\S]*?<\/td>/);
  assert.ok(actionsMatch, '缺少操作列');
  assert.doesNotMatch(actionsMatch[0], /data-action="toggle"/);
});

test('渠道卡片模板把冷却标记放到操作列上方而不是名称列', () => {
  const templateMatch = html.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.doesNotMatch(template, /<div class="ch-name-statuses">\s*\{\{\{cooldownBadge\}\}\}\s*<\/div>/);
  assert.match(template, /<td class="ch-col-actions"[^>]*>\s*<div class="ch-actions-stack">\s*<div class="ch-action-statuses">\s*\{\{\{cooldownBadge\}\}\}\s*<\/div>\s*<div class="ch-action-group">/);
});

test('渠道卡片模板为批量模型刷新结果预留行内状态槽', () => {
  const templateMatch = html.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.match(template, /<div class="ch-refresh-result-slot">\s*\{\{\{batchRefreshStatusHtml\}\}\}\s*<\/div>/);
});

test('操作列把冷却标记固定到右上角，移动端再退回普通流式布局', () => {
  const actionsColumnStyle = css.match(/\.ch-col-actions\s*\{[^}]+\}/);
  assert.ok(actionsColumnStyle, '缺少 .ch-col-actions 样式');
  assert.match(actionsColumnStyle[0], /position:\s*relative/);

  const stackStyle = css.match(/\.ch-actions-stack\s*\{[^}]+\}/);
  assert.ok(stackStyle, '缺少 .ch-actions-stack 样式');
  assert.doesNotMatch(stackStyle[0], /padding-top:/);

  const badgeStyle = css.match(/\.ch-action-statuses\s*\{[^}]+\}/);
  assert.ok(badgeStyle, '缺少 .ch-action-statuses 样式');
  assert.match(badgeStyle[0], /position:\s*absolute/);
  assert.match(badgeStyle[0], /top:\s*8px/);
  assert.match(badgeStyle[0], /right:\s*8px/);

  assert.match(css, /\.channel-table\s+\.ch-actions-stack\s*\{[\s\S]*?flex-direction:\s*column;[\s\S]*?align-items:\s*center;[\s\S]*?padding-top:\s*0;/);
  assert.match(css, /\.channel-table\s+\.ch-action-statuses\s*\{[\s\S]*?position:\s*static;[\s\S]*?justify-content:\s*center;/);
});

test('操作列为四个操作按钮保留足够宽度', () => {
  const actionsColumnStyle = css.match(/\.ch-col-actions\s*\{[^}]+\}/);
  assert.ok(actionsColumnStyle, '缺少 .ch-col-actions 样式');

  const styleBlock = actionsColumnStyle[0];
  assert.match(styleBlock, /width:\s*136px/);
  assert.match(styleBlock, /min-width:\s*136px/);
  assert.match(styleBlock, /max-width:\s*136px/);
});

test('启用列使用绿色和灰色开关样式', () => {
  const enabledColumnStyle = css.match(/\.ch-col-enabled\s*\{[^}]+\}/);
  assert.ok(enabledColumnStyle, '缺少 .ch-col-enabled 样式');
  assert.match(enabledColumnStyle[0], /width:\s*76px/);

  const switchStyle = css.match(/\.channel-enable-switch\s*\{[^}]+\}/);
  assert.ok(switchStyle, '缺少 .channel-enable-switch 样式');
  assert.match(switchStyle[0], /border-radius:\s*999px/);

  const onStyle = css.match(/\.channel-enable-switch--on\s*\{[^}]+\}/);
  assert.ok(onStyle, '缺少开关开启样式');
  assert.match(onStyle[0], /background:\s*#22c55e/);

  const offStyle = css.match(/\.channel-enable-switch--off\s*\{[^}]+\}/);
  assert.ok(offStyle, '缺少开关关闭样式');
  assert.match(offStyle[0], /background:\s*#cbd5e1/);
});

test('批量模型刷新结果支持行内状态、失败详情和操作按钮样式', () => {
  assert.match(css, /\.ch-refresh-result-slot\s*\{[\s\S]*?margin-top:\s*5px;/);
  assert.match(css, /\.channel-refresh-result--failed\s*\{[\s\S]*?display:\s*block;[\s\S]*?width:\s*min\(100%,\s*420px\);/);
  assert.match(css, /\.channel-refresh-result__detail\s+pre\s*\{[\s\S]*?max-height:\s*240px;[\s\S]*?overflow:\s*auto;/);
  assert.match(css, /\.channel-refresh-result-action\s*\{[\s\S]*?white-space:\s*nowrap;/);
});

test('排序卡片模板保留 data-channel-id 但不再显示渠道 ID 文案', () => {
  const templateMatch = html.match(/<template id="tpl-sort-item">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-sort-item 模板');

  const template = templateMatch[0];
  assert.match(template, /class="sort-item" data-channel-id="\{\{id\}\}"/);
  assert.doesNotMatch(template, /\(ID:\s*\{\{id\}\}\)/);
  assert.doesNotMatch(template, /sort-item-id/);
});
