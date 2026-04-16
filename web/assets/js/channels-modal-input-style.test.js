const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');
const zhLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');
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

test('模型配置行内操作按钮通过 CSS hover 切换样式，鼠标移出后恢复默认边框', () => {
  const deleteButtonStyle = css.match(/\.redirect-delete-btn\s*\{[^}]*width:\s*28px[^}]+\}/);
  assert.ok(deleteButtonStyle, '缺少 .redirect-delete-btn 样式');
  assert.match(deleteButtonStyle[0], /border:\s*1px solid var\(--neutral-200\)/);

  const deleteButtonHoverStyle = css.match(/\.redirect-delete-btn:hover\s*\{[^}]+\}/);
  assert.ok(deleteButtonHoverStyle, '缺少 .redirect-delete-btn:hover 样式');
  assert.match(deleteButtonHoverStyle[0], /background:\s*var\(--error-50\)/);
  assert.match(deleteButtonHoverStyle[0], /border-color:\s*var\(--error-500\)/);

  const lowercaseButtonHoverStyle = css.match(/\.redirect-lowercase-btn:hover\s*\{[^}]+\}/);
  assert.ok(lowercaseButtonHoverStyle, '缺少 .redirect-lowercase-btn:hover 样式');
  assert.match(lowercaseButtonHoverStyle[0], /background:\s*var\(--primary-50\)/);
  assert.match(lowercaseButtonHoverStyle[0], /border-color:\s*var\(--primary-500\)/);
  assert.match(lowercaseButtonHoverStyle[0], /color:\s*var\(--primary-600\)/);
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
  assert.match(headerBlockMatch[0], /align-items:\s*center/);
  assert.match(headerBlockMatch[0], /margin-bottom:\s*6px/);
});

test('编辑弹窗协议转换行与上游协议行保留显式垂直间距，避免两行贴在一起', () => {
  assert.match(html, /channel-editor-primary-row[\s\S]*channel-editor-primary-row/);

  const rowSpacingBlock = css.match(/\.channel-editor-group--primary\s*\{[^}]+\}/);
  assert.ok(rowSpacingBlock, '缺少主区块共享网格间距样式');
  assert.match(rowSpacingBlock[0], /row-gap:\s*8px/);
});

test('编辑弹窗第一行将渠道名称输入框与上游协议单选按视觉中心对齐', () => {
  assert.match(
    html,
    /<label class="form-label channel-editor-inline-label channel-editor-inline-label--muted"[\s\S]*?data-i18n="channels\.modal\.upstreamProtocol">上游协议<\/label>/
  );

  const alignedPrimaryFields = css.match(
    /\.channel-editor-primary-field--name,\s*[\r\n\s]*\.channel-editor-primary-field--type\s*\{[^}]+\}/
  );
  assert.ok(alignedPrimaryFields, '缺少第一行主区块字段对齐样式');
  assert.match(alignedPrimaryFields[0], /align-self:\s*center/);

  const inlineLabelBlock = css.match(/\.channel-editor-inline-label\s*\{[^}]+\}/);
  assert.ok(inlineLabelBlock, '缺少 .channel-editor-inline-label 样式');
  assert.match(inlineLabelBlock[0], /display:\s*inline-flex/);
  assert.match(inlineLabelBlock[0], /align-items:\s*center/);
});

test('协议转换提示文案统一为额外暴露协议表述', () => {
  assert.doesNotMatch(
    html,
    /<div class="models-hint" data-i18n="channels\.modal\.protocolTransformsHint">额外暴露协议,不含原生上游协议\(实验性\)<\/div>/
  );
  assert.match(zhLocale, /'channels\.modal\.protocolTransformsHint':\s*'额外暴露协议,不含原生上游协议\(实验性\)'/);
  assert.match(enLocale, /'channels\.modal\.protocolTransformsHint':\s*'Expose extra protocols only, excluding the native upstream protocol \(experimental\)'/);
});

test('协议转换提示改为 Gemini 选项后的内联小号提示，避免独占一行', () => {
  const hintBlock = css.match(/\.channel-editor-radio-hint\s*\{[^}]+\}/);
  assert.ok(hintBlock, '缺少 .channel-editor-radio-hint 样式');
  assert.match(hintBlock[0], /font-size:\s*12px/);
  assert.match(hintBlock[0], /color:\s*var\(--neutral-500\)/);

  const copyBlock = css.match(/\.channel-editor-radio-option-copy--with-hint\s*\{[^}]+\}/);
  assert.ok(copyBlock, '缺少 .channel-editor-radio-option-copy--with-hint 样式');
  assert.match(copyBlock[0], /display:\s*inline-flex/);
  assert.match(copyBlock[0], /flex-wrap:\s*wrap/);
  assert.match(copyBlock[0], /align-items:\s*baseline/);
});

test('编辑弹窗 API URL、API Key、模型配置标题块与按钮组按同一条中心线对齐', () => {
  assert.match(
    html,
    /class="channel-editor-section-header channel-editor-section-header--inline"[\s\S]*?channels\.apiUrl[\s\S]*?channel-editor-section-actions/
  );
  assert.match(
    html,
    /class="channel-editor-section-header"[\s\S]*?channels\.apiKey[\s\S]*?channel-editor-section-actions channel-editor-section-actions--keys/
  );
  assert.match(
    html,
    /class="channel-editor-section-header"[\s\S]*?channels\.modelConfig[\s\S]*?channel-editor-section-actions channel-editor-section-actions--models/
  );

  const headerBlock = css.match(/\.channel-editor-section-header\s*\{[^}]+\}/);
  assert.ok(headerBlock, '缺少 .channel-editor-section-header 样式');
  assert.match(headerBlock[0], /align-items:\s*center/);
});

test('编辑弹窗把协议转换和转换方式拆成第二主区块，两列分别对齐', () => {
  assert.match(html, /class="form-group channel-editor-group channel-editor-group--primary"/);
  assert.match(html, /class="channel-editor-primary-field channel-editor-primary-field--type"/);
  assert.match(html, /class="channel-editor-primary-field channel-editor-primary-field--transforms"/);
  assert.match(html, /class="channel-editor-primary-field channel-editor-primary-field--mode"/);

  const primaryGridBlock = css.match(/\.channel-editor-group--primary\s*\{[^}]+\}/);
  assert.ok(primaryGridBlock, '缺少共享主区块网格样式');
  assert.match(primaryGridBlock[0], /display:\s*grid/);
  assert.match(primaryGridBlock[0], /grid-template-columns:\s*minmax\(0,\s*1fr\)\s+minmax\(320px,\s*max-content\)/);

  const rowBlock = css.match(/\.channel-editor-group--primary\s+\.channel-editor-primary-row\s*\{[^}]+\}/);
  assert.ok(rowBlock, '缺少主区块行内容透传样式');
  assert.match(rowBlock[0], /display:\s*contents/);

  const rightColumnBlock = css.match(/\.channel-editor-primary-field--type,\s*[\r\n\s]*\.channel-editor-primary-field--mode\s*\{[^}]+\}/);
  assert.ok(rightColumnBlock, '缺少右侧协议列对齐样式');
  assert.match(rightColumnBlock[0], /justify-content:\s*flex-start/);
  assert.doesNotMatch(rightColumnBlock[0], /justify-content:\s*flex-end/);

  const transformsBlock = css.match(/\.channel-editor-primary-field--transforms\s*\{[^}]+\}/);
  assert.ok(transformsBlock, '缺少 .channel-editor-primary-field--transforms 样式');
  assert.match(transformsBlock[0], /flex-direction:\s*column/);
  assert.match(transformsBlock[0], /align-items:\s*flex-start/);

  const inlineGroupBlock = css.match(/\.channel-editor-inline-group\s*\{[^}]+\}/);
  assert.ok(inlineGroupBlock, '缺少 .channel-editor-inline-group 样式');
  assert.match(inlineGroupBlock[0], /display:\s*inline-flex/);
  assert.match(inlineGroupBlock[0], /align-items:\s*center/);
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
