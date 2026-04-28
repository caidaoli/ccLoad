const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');
const modalsJs = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

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

test('操作列为五个操作按钮保留足够宽度', () => {
  const actionsColumnStyle = css.match(/\.ch-col-actions\s*\{[^}]+\}/);
  assert.ok(actionsColumnStyle, '缺少 .ch-col-actions 样式');

  const styleBlock = actionsColumnStyle[0];
  assert.match(styleBlock, /width:\s*168px/);
  assert.match(styleBlock, /min-width:\s*168px/);
  assert.match(styleBlock, /max-width:\s*168px/);
});

test('批量模型刷新结果支持行内状态、失败详情和操作按钮样式', () => {
  assert.match(css, /\.ch-refresh-result-slot\s*\{[\s\S]*?margin-top:\s*5px;/);
  assert.match(css, /\.channel-refresh-result--failed\s*\{[\s\S]*?display:\s*block;[\s\S]*?width:\s*min\(100%,\s*420px\);/);
  assert.match(css, /\.channel-refresh-result__detail\s+pre\s*\{[\s\S]*?max-height:\s*240px;[\s\S]*?overflow:\s*auto;/);
  assert.match(css, /\.channel-refresh-result-action\s*\{[\s\S]*?white-space:\s*nowrap;/);
});

test('新一轮批量模型刷新开始前会先清空上一轮行内结果', () => {
  assert.match(
    modalsJs,
    /if \(mode === 'replace'[\s\S]*?return;\s*\}\s*[\r\n]+\s*if \(typeof clearAllBatchRefreshResults === 'function'\) \{\s*clearAllBatchRefreshResults\(\);\s*\}[\s\S]*?const actionBtnIDs = \['batchRefreshMergeBtn'/m
  );
});

test('排序卡片模板保留 data-channel-id 但不再显示渠道 ID 文案', () => {
  const templateMatch = html.match(/<template id="tpl-sort-item">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-sort-item 模板');

  const template = templateMatch[0];
  assert.match(template, /class="sort-item" data-channel-id="\{\{id\}\}"/);
  assert.doesNotMatch(template, /\(ID:\s*\{\{id\}\}\)/);
  assert.doesNotMatch(template, /sort-item-id/);
});
