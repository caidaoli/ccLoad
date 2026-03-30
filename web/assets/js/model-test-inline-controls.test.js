const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'model-test.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const sharedCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');

test('model-test 页静态控件不再使用内联事件', () => {
  assert.doesNotMatch(html, /onclick="(?:setTestMode|selectAllModels|deselectAllModels|fetchAndAddModels|deleteSelectedModels|runModelTests)\([^"]*\)"/);
  assert.doesNotMatch(html, /onchange="toggleAllModels\(this\.checked\)"/);
  assert.match(html, /data-action="set-test-mode"/);
  assert.match(html, /data-action="select-all-models"/);
  assert.match(html, /data-action="deselect-all-models"/);
  assert.match(html, /data-action="fetch-and-add-models"/);
  assert.match(html, /data-action="delete-selected-models"/);
  assert.match(html, /data-action="run-model-tests"/);
  assert.match(html, /data-change-action="toggle-all-models"/);
});

test('model-test 页接入日志页同款渠道编辑器桥接并将渠道名渲染为可点击按钮', () => {
  assert.match(html, /<link rel="stylesheet" href="\/web\/assets\/css\/channels\.css\?v=__VERSION__">/);
  assert.match(html, /<script defer src="\/web\/assets\/js\/logs-channel-editor\.js\?v=__VERSION__"><\/script>/);
  assert.match(html, /<button type="button" class="channel-link" data-channel-id="{{channelId}}" title="{{channelName}}">{{channelName}}<\/button>/);
});

test('model-test.js 使用集中绑定处理页面控件和重渲染表头复选框', () => {
  assert.match(script, /window\.initPageBootstrap\(\{/);
  assert.match(script, /topbarKey:\s*'model-test'/);
  assert.match(script, /function initModelTestActions\(\)/);
  assert.match(script, /window\.initDelegatedActions\(\{/);
  assert.match(script, /boundKey:\s*'modelTestActionsBound'/);
  assert.match(script, /'set-test-mode':\s*\(actionTarget\)\s*=> setTestMode\(actionTarget\.dataset\.mode \|\| ''\)/);
  assert.match(script, /'select-all-models':\s*\(\)\s*=> selectAllModels\(\)/);
  assert.match(script, /'deselect-all-models':\s*\(\)\s*=> deselectAllModels\(\)/);
  assert.match(script, /'fetch-and-add-models':\s*\(\)\s*=> fetchAndAddModels\(\)/);
  assert.match(script, /'delete-selected-models':\s*\(\)\s*=> deleteSelectedModels\(\)/);
  assert.match(script, /'run-model-tests':\s*\(\)\s*=> runModelTests\(\)/);
  assert.match(script, /'toggle-all-models':\s*\(actionTarget\)\s*=> toggleAllModels\(actionTarget\.checked\)/);
  assert.match(script, /data-change-action="toggle-all-models"/);
  assert.doesNotMatch(script, /onchange="toggleAllModels/);
  assert.match(script, /const toolbar = document\.querySelector\('\.model-test-toolbar'\);/);
  assert.match(script, /toolbar\?\.classList\.toggle\('model-test-toolbar--model-mode',\s*isModelMode\)/);
  assert.match(script, /bootstrap\(\);/);
});

test('model-test.js 在按模型测试模式下将渠道按钮点击委托到编辑弹窗', () => {
  assert.match(script, /const channelBtn = event\.target\.closest\('\.channel-link\[data-channel-id\]'\);/);
  assert.match(script, /if \(testMode !== TEST_MODE_MODEL \|\| !channelBtn\) return;/);
  assert.match(script, /openLogChannelEditor\(channelId\)/);
});

test('model-test 页渠道按钮去掉默认按钮边框和底色', () => {
  assert.match(sharedCss, /\.model-test-table\s+\.channel-link\s*\{[\s\S]*?padding:\s*0;[\s\S]*?border:\s*none;[\s\S]*?background:\s*transparent;/);
});
