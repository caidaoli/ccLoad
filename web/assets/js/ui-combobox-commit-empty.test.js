const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const channelsFiltersSource = fs.readFileSync(path.join(__dirname, 'channels-filters.js'), 'utf8');
const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

test('createSearchableCombobox 暴露 commitEmptyAsFirst 选项，并在空输入时提交第一项', () => {
  // 选项已通过 destructure 接入
  assert.match(uiSource, /commitEmptyAsFirst\s*=\s*false/);
  // 空输入分支按选项提交第一项（getOptions()[0]）
  assert.match(
    uiSource,
    /if\s*\(\s*commitEmptyAsFirst\s*\)\s*\{[\s\S]*?const\s+opts\s*=\s*getOptions\(\);[\s\S]*?commitValue\(\s*opts\[0\]\.value,\s*opts\[0\]\.label\s*\)/
  );
});

test('渠道页模型与渠道名筛选启用 commitEmptyAsFirst（空回车选回“全部”）', () => {
  assert.match(channelsFiltersSource, /inputId:\s*'modelFilter'[\s\S]*?commitEmptyAsFirst:\s*true/);
  assert.match(channelsFiltersSource, /inputId:\s*'searchInput'[\s\S]*?commitEmptyAsFirst:\s*true/);
});

test('stats 页模型与渠道名筛选允许提交部分输入并保留空值回到全部', () => {
  assert.match(statsSource, /inputId:\s*'f_name'[\s\S]*?allowCustomInput:\s*true[\s\S]*?commitEmptyAsFirst:\s*true/);
  assert.match(statsSource, /inputId:\s*'f_model'[\s\S]*?allowCustomInput:\s*true[\s\S]*?commitEmptyAsFirst:\s*true/);
});

test('logs 页模型与渠道名筛选允许提交部分输入并保留空值回到全部', () => {
  assert.match(logsSource, /inputId:\s*'f_name'[\s\S]*?allowCustomInput:\s*true[\s\S]*?commitEmptyAsFirst:\s*true/);
  assert.match(logsSource, /inputId:\s*'f_model'[\s\S]*?allowCustomInput:\s*true[\s\S]*?commitEmptyAsFirst:\s*true/);
});
