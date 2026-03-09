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

test('操作列为五个操作按钮保留足够宽度', () => {
  const actionsColumnStyle = css.match(/\.ch-col-actions\s*\{[^}]+\}/);
  assert.ok(actionsColumnStyle, '缺少 .ch-col-actions 样式');

  const styleBlock = actionsColumnStyle[0];
  assert.match(styleBlock, /width:\s*220px/);
  assert.match(styleBlock, /min-width:\s*220px/);
  assert.match(styleBlock, /max-width:\s*220px/);
});
