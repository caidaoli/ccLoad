const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const channelsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const channelsInitScript = fs.readFileSync(path.join(__dirname, 'channels-init.js'), 'utf8');
const channelsRenderScript = fs.readFileSync(path.join(__dirname, 'channels-render.js'), 'utf8');
const channelsUrlsScript = fs.readFileSync(path.join(__dirname, 'channels-urls.js'), 'utf8');
const channelsModalsScript = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');
const channelsTestScript = fs.readFileSync(path.join(__dirname, 'channels-test.js'), 'utf8');

test('channels-render.js 不再拼表头全选框 onchange', () => {
  assert.doesNotMatch(channelsRenderScript, /onchange="toggleVisibleChannelsSelection\(\)"/);
  assert.match(channelsRenderScript, /data-change-action="toggle-visible-channels-selection"/);
  assert.match(channelsRenderScript, /const headerCheckbox = e\.target\.closest\('#visibleSelectionCheckbox'\);/);
  assert.match(channelsRenderScript, /if \(headerCheckbox\) \{/);
  assert.match(channelsRenderScript, /toggleVisibleChannelsSelection\(\);/);
});

test('channels-test.js 不再拼测试结果折叠按钮 onclick', () => {
  assert.doesNotMatch(channelsTestScript, /onclick="toggleResponse\('/);
  assert.match(channelsTestScript, /data-action="toggle-response"/);
  assert.match(channelsTestScript, /data-response-target="\$\{contentId\}"/);
});

test('channels-init.js 集中处理测试结果折叠动作', () => {
  assert.match(channelsInitScript, /window\.initDelegatedActions\(\{/);
  assert.match(channelsInitScript, /boundKey:\s*'channelsPageActionsBound'/);
  assert.match(channelsInitScript, /'toggle-response':\s*\(actionTarget\)\s*=>/);
  assert.match(channelsInitScript, /const responseTarget = actionTarget\.dataset\.responseTarget;/);
  assert.match(channelsInitScript, /window\.toggleResponse\(responseTarget\);/);
});

test('channels.html 的 URL 与模型动态模板不再使用 inline 事件', () => {
  assert.doesNotMatch(channelsHtml, /onchange="toggleURLSelection\(\{\{index\}\}, this\.checked\)"/);
  assert.doesNotMatch(channelsHtml, /onchange="updateInlineURL\(\{\{index\}\}, this\.value\)"/);
  assert.doesNotMatch(channelsHtml, /onclick="testInlineURL\(\{\{index\}\}, this\)"/);
  assert.doesNotMatch(channelsHtml, /onclick="deleteInlineURL\(\{\{index\}\}\)"/);
  assert.doesNotMatch(channelsHtml, /onchange="toggleModelSelection\(\{\{index\}\}, this\.checked\)"/);
  assert.match(channelsHtml, /class="inline-url-test-btn" data-index="\{\{index\}\}"/);
  assert.match(channelsHtml, /class="inline-url-delete-btn" data-index="\{\{index\}\}"/);
});

test('channels-urls.js 通过 URL 表体委托处理动态行交互', () => {
  assert.match(channelsUrlsScript, /function initInlineURLTableEventDelegation\(\)/);
  assert.match(channelsUrlsScript, /const tbody = document\.getElementById\('inlineUrlTableBody'\);/);
  assert.match(channelsUrlsScript, /tbody\.addEventListener\('change', \(e\) => \{/);
  assert.match(channelsUrlsScript, /const checkbox = e\.target\.closest\('\.url-checkbox'\);/);
  assert.match(channelsUrlsScript, /toggleURLSelection\(index, checkbox\.checked\);/);
  assert.match(channelsUrlsScript, /const input = e\.target\.closest\('\.inline-url-input'\);/);
  assert.match(channelsUrlsScript, /updateInlineURL\(index, input\.value\);/);
  assert.match(channelsUrlsScript, /tbody\.addEventListener\('click', \(e\) => \{/);
  assert.match(channelsUrlsScript, /const testBtn = e\.target\.closest\('\.inline-url-test-btn'\);/);
  assert.match(channelsUrlsScript, /testInlineURL\(index, testBtn\);/);
  assert.match(channelsUrlsScript, /const deleteBtn = e\.target\.closest\('\.inline-url-delete-btn'\);/);
  assert.match(channelsUrlsScript, /deleteInlineURL\(index\);/);
});

test('channels 添加弹窗在 URL 区域内提前提示重复渠道', () => {
  assert.match(channelsHtml, /id="channelDuplicateHint" class="channel-duplicate-hint" hidden role="status" aria-live="polite"/);
  assert.match(channelsUrlsScript, /scheduleChannelDuplicateHintCheck\(\);/);
  assert.match(channelsModalsScript, /function scheduleChannelDuplicateHintCheck\(\)/);
  assert.match(channelsModalsScript, /function renderChannelDuplicateHint\(dupes\)/);
  assert.match(channelsModalsScript, /channelTypeRadios\.addEventListener\('change', \(event\) => \{[\s\S]*scheduleChannelDuplicateHintCheck\(\);/);
});

test('URL 输入变更会同步 exact URL 对转换方式的限制', () => {
  assert.match(channelsModalsScript, /function syncProtocolTransformModeForURLs\(\)/);
  assert.match(channelsUrlsScript, /function updateInlineURL\(index, value\)[\s\S]*syncProtocolTransformModeForURLs\(\);/);
  assert.match(channelsUrlsScript, /function renderInlineURLTable\(\)[\s\S]*syncProtocolTransformModeForURLs\(\);/);
});

test('channels-modals.js 在 redirect 表体委托处理模型复选框', () => {
  assert.match(channelsModalsScript, /const checkbox = e\.target\.closest\('\.model-checkbox'\);/);
  assert.match(channelsModalsScript, /toggleModelSelection\(index, checkbox\.checked\);/);
});

test('channels-modals.js 不再手工写模型行按钮 hover 样式，避免鼠标移出后残留边框', () => {
  assert.doesNotMatch(channelsModalsScript, /tbody\.addEventListener\('mouseover', \(e\) => \{/);
  assert.doesNotMatch(channelsModalsScript, /tbody\.addEventListener\('mouseout', \(e\) => \{/);
  assert.doesNotMatch(channelsModalsScript, /deleteBtn\.style\.borderColor = 'var\(--error-500\)'/);
  assert.doesNotMatch(channelsModalsScript, /deleteBtn\.style\.borderColor = 'var\(--error-300\)'/);
  assert.doesNotMatch(channelsModalsScript, /lowercaseBtn\.style\.borderColor = 'var\(--primary-500\)'/);
});
