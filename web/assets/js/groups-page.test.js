const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const webRoot = path.join(__dirname, '..', '..');
const jsRoot = path.join(webRoot, 'assets', 'js');

test('groups.html 挂载分组页面容器和编辑弹窗', () => {
  const html = fs.readFileSync(path.join(webRoot, 'groups.html'), 'utf8');

  assert.match(html, /data-i18n="groups\.title"/);
  assert.match(html, />模型管理 - Claude Code & Codex Proxy</);
  assert.match(html, /id="addGroupBtn"/);
  assert.match(html, /id="groups-container"/);
  assert.match(html, /id="groupModal"/);
  assert.match(html, /id="groupMatchRegex"/);
  assert.match(html, /id="groupFirstTokenTimeout"/);
  assert.match(html, /id="groupSessionKeepTime"/);
  assert.match(html, /id="groupModeButtons"/);
  assert.match(html, /data-action="select-group-mode"/);
  assert.doesNotMatch(html, /<select[^>]+id="groupMode"/);
  assert.match(html, /id="groupPickerPanel"/);
  assert.match(html, /id="groupSelectedPanel"/);
  assert.match(html, /class="groups-form-actions"/);
  assert.match(html, /assets\/js\/groups\.js/);
});

test('分组编辑弹窗允许滚动并固定底部保存区，避免按钮被裁掉', () => {
  const html = fs.readFileSync(path.join(webRoot, 'groups.html'), 'utf8');

  assert.match(html, /\.groups-form\s*\{[\s\S]*?overflow:\s*auto;/);
  assert.match(html, /\.groups-form-actions\s*\{[\s\S]*?position:\s*sticky;[\s\S]*?bottom:\s*0;/);
});

test('左侧模型列表容器会撑满剩余高度，避免只剩线条看不到模型', () => {
  const html = fs.readFileSync(path.join(webRoot, 'groups.html'), 'utf8');

  assert.match(html, /\.groups-panel-body\s*\{[\s\S]*?flex:\s*1(?:\s+1\s+auto)?;/);
});

test('顶部导航包含模型管理入口', () => {
  const ui = fs.readFileSync(path.join(jsRoot, 'ui.js'), 'utf8');
  const zhCN = fs.readFileSync(path.join(webRoot, 'assets', 'locales', 'zh-CN.js'), 'utf8');
  const en = fs.readFileSync(path.join(webRoot, 'assets', 'locales', 'en.js'), 'utf8');

  assert.match(ui, /key:\s*'groups'/);
  assert.match(ui, /labelKey:\s*'nav\.groups'/);
  assert.match(ui, /href:\s*'\/web\/groups\.html'/);
  assert.match(zhCN, /'nav\.groups': '模型管理'/);
  assert.match(en, /'nav\.groups': 'Model Management'/);
});

test('groups.js 使用共享 bootstrap 并接入分组管理接口', () => {
  const script = fs.readFileSync(path.join(jsRoot, 'groups.js'), 'utf8');

  assert.match(script, /window\.initPageBootstrap\(\{/);
  assert.match(script, /topbarKey:\s*'groups'/);
  assert.match(script, /select-group-mode/);
  assert.match(script, /fetchDataWithAuth\('\/admin\/groups'\)/);
  assert.match(script, /fetchDataWithAuth\('\/admin\/groups\/model-options'\)/);
  assert.match(script, /match_regex/);
  assert.match(script, /first_token_time_out/);
  assert.match(script, /session_keep_time/);
  assert.match(script, /auto-add-group-items/);
  assert.match(script, /dragstart/);
  assert.match(script, /dragover/);
  assert.match(script, /drop/);
  assert.match(script, /draggable="true"/);
  assert.doesNotMatch(script, /move-group-item/);
  assert.match(script, /method:\s*'POST'/);
  assert.match(script, /method:\s*'PUT'/);
  assert.match(script, /method:\s*'DELETE'/);
});

test('分组页语言包包含高级字段和新交互文案', () => {
  const zhCN = fs.readFileSync(path.join(webRoot, 'assets', 'locales', 'zh-CN.js'), 'utf8');
  const en = fs.readFileSync(path.join(webRoot, 'assets', 'locales', 'en.js'), 'utf8');

  [
    'groups.field.matchRegex',
    'groups.field.firstTokenTimeout',
    'groups.field.sessionKeepTime',
    'groups.field.availableModels',
    'groups.field.searchModels',
    'groups.actions.autoAdd',
    'groups.actions.moveUp',
    'groups.actions.moveDown',
    'groups.hint.matchRegex',
    'groups.hint.firstTokenTimeout',
    'groups.hint.sessionKeepTime',
    'groups.messages.invalidRegex',
    'groups.messages.noAutoAddMatch'
  ].forEach((key) => {
    assert.match(zhCN, new RegExp(`'${key.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}'`));
    assert.match(en, new RegExp(`'${key.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}'`));
  });

  assert.match(zhCN, /'groups\.title': '模型管理 - Claude Code & Codex Proxy'/);
  assert.match(zhCN, /'groups\.add': '\+ 添加模型'/);
  assert.match(zhCN, /'groups\.field\.name': '模型名'/);
  assert.match(zhCN, /'groups\.field\.items': '模型成员'/);
  assert.match(zhCN, /'groups\.modal\.createTitle': '添加模型'/);
  assert.match(zhCN, /'groups\.modal\.editTitle': '编辑模型'/);
  assert.match(en, /'groups\.title': 'Model Management - Claude Code & Codex Proxy'/);
  assert.match(en, /'groups\.add': '\+ Add Model'/);
  assert.match(en, /'groups\.field\.name': 'Model Name'/);
  assert.match(en, /'groups\.field\.items': 'Model Members'/);
  assert.match(en, /'groups\.modal\.createTitle': 'Add Model'/);
  assert.match(en, /'groups\.modal\.editTitle': 'Edit Model'/);
});
