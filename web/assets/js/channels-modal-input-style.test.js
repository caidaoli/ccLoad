const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');
const urlScript = fs.readFileSync(path.join(__dirname, 'channels-urls.js'), 'utf8');

test('编辑弹窗动态输入框复用统一浅色输入样式类', () => {
  const requiredClasses = [
    /class="inline-key-input\s+modal-inline-input"/,
    /class="inline-url-input\s+modal-inline-input"/,
    /class="redirect-from-input\s+modal-inline-input"/,
    /class="redirect-to-input\s+modal-inline-input"/
  ];

  requiredClasses.forEach((pattern) => {
    assert.match(html, pattern);
  });
});

test('统一浅色输入样式显式锁定背景和文字颜色', () => {
  const styleBlockMatch = css.match(/\.modal-inline-input\s*\{[^}]+\}/);
  assert.ok(styleBlockMatch, '缺少 .modal-inline-input 样式');

  const styleBlock = styleBlockMatch[0];
  assert.match(styleBlock, /background:\s*rgba\(255,\s*255,\s*255,\s*0\.9\)/);
  assert.match(styleBlock, /color:\s*var\(--neutral-900\)/);
  assert.match(styleBlock, /color-scheme:\s*light/);
});

test('测试渠道模型下拉显式锁定文字颜色和浅色控件配色', () => {
  const styleBlockMatch = css.match(/\.model-select\s*\{[^}]+\}/);
  assert.ok(styleBlockMatch, '缺少 .model-select 样式');

  const styleBlock = styleBlockMatch[0];
  assert.match(styleBlock, /color:\s*var\(--neutral-900\)/);
  assert.match(styleBlock, /color-scheme:\s*light/);
});

test('编辑弹窗 Key 状态筛选下拉复用统一浅色选择框样式', () => {
  assert.match(html, /<select id="keyStatusFilter"[^>]*class="modal-inline-select"[^>]*>/);

  const styleBlockMatch = css.match(/\.modal-inline-select\s*\{[^}]+\}/);
  assert.ok(styleBlockMatch, '缺少 .modal-inline-select 样式');

  const styleBlock = styleBlockMatch[0];
  assert.match(styleBlock, /background:\s*rgba\(255,\s*255,\s*255,\s*0\.9\)/);
  assert.match(styleBlock, /color:\s*var\(--neutral-900\)/);
  assert.match(styleBlock, /color-scheme:\s*light/);
  assert.match(styleBlock, /-webkit-text-fill-color:\s*var\(--neutral-900\)/);
});

test('URL 统计列使用紧凑列宽样式，避免挤压 API URL 列', () => {
  assert.match(urlScript, /statusTh\.className = 'url-stats-th inline-url-col-status'/);
  assert.match(urlScript, /latencyTh\.className = 'url-stats-th inline-url-col-latency'/);

  const statusColumnStyle = css.match(/\.inline-url-col-status\s*\{[^}]+\}/);
  assert.ok(statusColumnStyle, '缺少 .inline-url-col-status 样式');
  assert.match(statusColumnStyle[0], /width:\s*72px/);

  const latencyColumnStyle = css.match(/\.inline-url-col-latency\s*\{[^}]+\}/);
  assert.ok(latencyColumnStyle, '缺少 .inline-url-col-latency 样式');
  assert.match(latencyColumnStyle[0], /width:\s*60px/);
});

test('编辑弹窗 Key 策略与 Key 数量同行展示，避免单独换行', () => {
  assert.match(html, /channel-editor-section-title--key/);
  assert.match(html, /channel-editor-inline-strategy/);
  assert.match(html, /channel-editor-section-title--key[\s\S]*?id="inlineKeyCount"[\s\S]*?channel-editor-inline-strategy[\s\S]*?id="keyStrategyRadios"/);

  const keyTitleStyle = css.match(/\.channel-editor-section-title--key\s*\{[^}]+\}/);
  assert.ok(keyTitleStyle, '缺少 .channel-editor-section-title--key 样式');
  assert.match(keyTitleStyle[0], /flex-wrap:\s*nowrap/);

  const inlineStrategyStyle = css.match(/\.channel-editor-inline-strategy\s*\{[^}]+\}/);
  assert.ok(inlineStrategyStyle, '缺少 .channel-editor-inline-strategy 样式');
  assert.match(inlineStrategyStyle[0], /display:\s*inline-flex/);
  assert.match(inlineStrategyStyle[0], /align-items:\s*center/);
});

test('编辑弹窗主区块间距收紧，减少名称、URL、Key、模型配置之间的空隙', () => {
  const formBlockMatch = css.match(/\.channel-editor-form\s*\{[^}]+\}/);
  assert.ok(formBlockMatch, '缺少 .channel-editor-form 样式');
  assert.match(formBlockMatch[0], /gap:\s*14px/);

  const headerBlockMatch = css.match(/\.channel-editor-section-header\s*\{[^}]+\}/);
  assert.ok(headerBlockMatch, '缺少 .channel-editor-section-header 样式');
  assert.match(headerBlockMatch[0], /margin-bottom:\s*6px/);
});

test('编辑弹窗协议转换行与上游协议行保留显式垂直间距，避免两行贴在一起', () => {
  assert.match(html, /channel-editor-primary-row[\s\S]*channel-editor-primary-row/);

  const rowSpacingBlock = css.match(/\.channel-editor-primary-row\s*\+\s*\.channel-editor-primary-row\s*\{[^}]+\}/);
  assert.ok(rowSpacingBlock, '缺少主区块连续行间距样式');
  assert.match(rowSpacingBlock[0], /margin-top:\s*8px/);
});

test('编辑弹窗协议转换标签复用渠道名称的主标签样式，避免视觉不对齐', () => {
  assert.match(html, /<label class="form-label channel-editor-inline-label" data-i18n="channels\.channelName">渠道名称/);
  assert.match(html, /<label class="form-label channel-editor-inline-label"[\s\S]*?data-i18n="channels\.modal\.protocolTransforms">协议转换<\/label>/);
  assert.doesNotMatch(html, /data-i18n="channels\.modal\.protocolTransforms">额外协议转换<\/label>/);
  assert.doesNotMatch(html, /<label class="[^"]*channel-editor-inline-label--muted[^"]*"[^>]*data-i18n="channels\.modal\.protocolTransforms">/);
});

test('API URL 表格列间距减半，调度提示文字降一号', () => {
  assert.match(html, /class="inline-table mobile-inline-table inline-url-table"/);

  const urlTableHeadBlock = css.match(/\.inline-url-table th\s*\{[^}]+\}/);
  assert.ok(urlTableHeadBlock, '缺少 .inline-url-table th 样式');
  assert.match(urlTableHeadBlock[0], /padding:\s*6px 5px/);

  const urlTableCellBlock = css.match(/\.inline-url-table td\s*\{[^}]+\}/);
  assert.ok(urlTableCellBlock, '缺少 .inline-url-table td 样式');
  assert.match(urlTableCellBlock[0], /padding:\s*4px 4px/);

  const hintBlock = css.match(/\.inline-url-header-hint\s*\{[^}]+\}/);
  assert.ok(hintBlock, '缺少 .inline-url-header-hint 样式');
  assert.match(hintBlock[0], /font-size:\s*12px/);
});
