const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'tokens.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const zh = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const en = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

test('tokens 编辑弹窗新增渠道限制区域并使用 90% 桌面宽度和限制区双列布局', () => {
  assert.match(html, /<div class="modal-content modal-content--wide token-edit-modal">/);
  assert.match(html, /<div class="modal-body token-edit-body token-edit-layout">/);
  assert.match(html, /<div class="token-edit-sidebar">[\s\S]*token-edit-section--basic[\s\S]*token-edit-section--quota[\s\S]*<\/div>/);
  assert.match(html, /<div class="token-edit-main">[\s\S]*token-edit-section--channels[\s\S]*token-edit-section--models[\s\S]*<\/div>/);
  assert.match(html, /data-token-edit-section="channels"/);
  assert.match(html, /id="editAllowedChannelsCount"/);
  assert.match(html, /id="allowedChannelsTableBody"/);
  assert.match(html, /data-action="show-channel-select-modal"/);
  assert.match(html, /data-action="batch-delete-allowed-channels"/);
  assert.match(css, /\.modal-content--wide\s*\{[\s\S]*?width:\s*90%;[\s\S]*?max-width:\s*none;/);
  assert.match(css, /\.token-edit-layout\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*320px minmax\(0,\s*1fr\);/);
  assert.match(css, /\.token-edit-main\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\) minmax\(0,\s*1fr\);/);
  assert.match(css, /\.token-edit-section--channels\s*\{[\s\S]*?flex:\s*1 1 auto;[\s\S]*?min-height:\s*0;/);
  assert.match(css, /\.token-edit-channels-table\s*\{[\s\S]*?flex:\s*1 1 auto;[\s\S]*?min-height:\s*0;[\s\S]*?overflow-y:\s*auto;/);
});

test('tokens 移动端编辑弹窗退化为纵向 B 方案', () => {
  assert.match(css, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.modal-content--wide\s*\{[\s\S]*?width:\s*min\(720px,\s*calc\(100vw - 24px\)\);/);
  assert.match(css, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-layout\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-direction:\s*column;/);
});

test('tokens.js 保存并渲染 allowed_channel_ids', () => {
  assert.match(script, /let editAllowedChannelIDs = \[\];/);
  assert.match(script, /let selectedAllowedChannelIDs = new Set\(\);/);
  assert.match(script, /function renderAllowedChannelsTable\(\)/);
  assert.match(script, /editAllowedChannelIDs = \(token\.allowed_channel_ids \|\| \[\]\)\.slice\(\);/);
  assert.match(script, /allowed_channel_ids:\s*editAllowedChannelIDs,/);
  assert.match(script, /'show-channel-select-modal':\s*\(\)\s*=> showChannelSelectModal\(\)/);
  assert.match(script, /'confirm-channel-selection':\s*\(\)\s*=> confirmChannelSelection\(\)/);
  assert.match(script, /'batch-delete-allowed-channels':\s*\(\)\s*=> batchDeleteSelectedAllowedChannels\(\)/);
  assert.match(script, /'toggle-allowed-channel':\s*\(actionTarget\)\s*=>/);
});

test('tokens 模型选择按当前渠道限制聚合可选模型', () => {
  assert.match(script, /function getAvailableModelsForCurrentChannelRestriction\(\)/);
  assert.match(script, /if \(editAllowedChannelIDs\.length === 0\) \{[\s\S]*?return availableModelsCache;/);
  assert.match(script, /const allowedChannelIDs = new Set\(editAllowedChannelIDs\);/);
  assert.match(script, /allChannels\.forEach\(ch => \{[\s\S]*?if \(!allowedChannelIDs\.has\(normalizeChannelID\(ch\.id\)\)\) return;/);
  assert.match(script, /const sourceModels = getAvailableModelsForCurrentChannelRestriction\(\);[\s\S]*?let models = sourceModels\.filter/);
  assert.match(script, /const isEmptyCache = sourceModels\.length === 0;/);
});

test('tokens 渠道限制文案已本地化', () => {
  for (const locale of [zh, en]) {
    assert.match(locale, /'tokens\.channelRestriction':/);
    assert.match(locale, /'tokens\.channelCountSuffix':/);
    assert.match(locale, /'tokens\.selectChannelTitle':/);
    assert.match(locale, /'tokens\.noChannelRestriction':/);
    assert.match(locale, /'tokens\.msg\.selectAtLeastOneChannel':/);
  }
});
