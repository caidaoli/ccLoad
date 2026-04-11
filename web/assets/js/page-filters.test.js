const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const pageFiltersSource = fs.readFileSync(path.join(__dirname, 'page-filters.js'), 'utf8');
const logsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const statsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const trendHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'trend.html'), 'utf8');

function loadPageFilters() {
  const sandbox = {
    console,
    window: {},
    document: {
      querySelectorAll() {
        return [];
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(pageFiltersSource, sandbox);
  return sandbox.window.PageFilters;
}

test('page-filters 渲染 logs 布局时保留专用 class 和关键筛选控件 id', () => {
  const pageFilters = loadPageFilters();
  const html = pageFilters.renderLayout('logs');

  assert.match(html, /class="filter-bar logs-filter-bar mt-2"/);
  assert.match(html, /class="filter-controls logs-filter-controls"/);
  assert.match(html, /class="filter-group logs-filter-group"/);
  assert.match(html, /class="filter-info logs-filter-info"/);
  assert.match(html, /class="filter-actions filter-actions--page logs-filter-actions"/);
  assert.match(html, /class="logs-filter-summary-row"[\s\S]*id="displayedCount"[\s\S]*id="btn_filter"/);
  assert.match(html, /id="f_channel_type"/);
  assert.match(html, /id="f_hours"/);
  // 渠道ID已移除，渠道名与模型均改为 combobox
  assert.doesNotMatch(html, /id="f_id"/);
  assert.match(html, /id="f_name" class="filter-select filter-combobox"/);
  assert.match(html, /id="f_name_dropdown" class="filter-dropdown"/);
  assert.match(html, /id="f_model" class="filter-select filter-combobox"/);
  assert.match(html, /id="f_model_dropdown" class="filter-dropdown"/);
  assert.doesNotMatch(html, /data-i18n="trend\.allModels"/);
  assert.match(html, /id="f_log_source"/);
  assert.doesNotMatch(html, /value="scheduled_check"/);
  assert.doesNotMatch(html, /value="manual_test"/);
  assert.match(html, /id="f_status"/);
  assert.match(html, /id="f_auth_token"/);
  assert.match(html, /id="btn_filter"/);
});

test('page-filters 渲染 stats/trend 布局时保留各自特有控件', () => {
  const pageFilters = loadPageFilters();
  const statsLayout = pageFilters.renderLayout('stats');
  const trendLayout = pageFilters.renderLayout('trend');

  assert.match(statsLayout, /class="filter-bar stats-filter-bar mt-2"/);
  assert.match(statsLayout, /class="filter-controls stats-filter-controls"/);
  assert.match(statsLayout, /class="stats-filter-summary-row"/);
  assert.match(statsLayout, /class="filter-group filter-group--checkbox stats-filter-group stats-filter-group--checkbox"/);
  assert.match(statsLayout, /class="filter-info stats-filter-info"/);
  assert.match(statsLayout, /class="filter-actions filter-actions--page stats-filter-actions"/);
  assert.match(statsLayout, /id="f_hide_zero_success"/);
  assert.match(statsLayout, /id="statsCount"/);
  assert.match(trendLayout, /id="f_model" class="filter-select(?:\s+[^"]+)?"/);
  assert.match(trendLayout, /data-i18n="trend\.allModels"/);
  assert.doesNotMatch(trendLayout, /id="f_hide_zero_success"/);
  // trend 渠道ID和渠道名改为 combobox 结构
  assert.match(trendLayout, /id="f_id" class="filter-select filter-combobox"/);
  assert.match(trendLayout, /id="f_id_dropdown" class="filter-dropdown"/);
  assert.match(trendLayout, /id="f_name" class="filter-select filter-combobox"/);
  assert.match(trendLayout, /id="f_name_dropdown" class="filter-dropdown"/);

  // stats 模型筛选使用 combobox 结构
  assert.match(statsLayout, /id="f_model" class="filter-select filter-combobox"/);
  assert.match(statsLayout, /id="f_model_dropdown" class="filter-dropdown"/);
  assert.doesNotMatch(statsLayout, /<input[^>]*id="f_model"[^>]*type="text"[^>]*data-i18n-placeholder/);
});

test('page-filters 使用响应式宽度类代替筛选控件内联像素宽度', () => {
  const pageFilters = loadPageFilters();
  const logsLayout = pageFilters.renderLayout('logs');
  const statsLayout = pageFilters.renderLayout('stats');
  const trendLayout = pageFilters.renderLayout('trend');

  [logsLayout, statsLayout, trendLayout].forEach((layout) => {
    assert.doesNotMatch(layout, /style="[^"]*(?:min-width|max-width)\s*:/);
    assert.doesNotMatch(layout, /style="[^"]*(?:padding|font-size|flex)\s*:/);
  });

  assert.match(statsLayout, /id="f_channel_type" class="filter-select filter-control--compact"/);
  assert.match(statsLayout, /id="f_hours" class="filter-select filter-control--compact filter-control--time-range"/);
  // stats 渠道名改为 combobox，渠道 ID 筛选已移除
  assert.match(statsLayout, /id="f_name" class="filter-select filter-combobox"/);
  assert.match(statsLayout, /id="f_name_dropdown" class="filter-dropdown"/);
  assert.doesNotMatch(statsLayout, /id="f_id"/);
  assert.match(logsLayout, /class="filter-group logs-filter-group logs-filter-group--range"[\s\S]*id="f_hours" class="filter-select filter-control--compact filter-control--time-range logs-filter-control--range"/);
  // 渠道ID已从日志页移除，渠道名与模型均改为 combobox
  assert.doesNotMatch(logsLayout, /id="f_id"/);
  assert.match(logsLayout, /id="f_name" class="filter-select filter-combobox"/);
  assert.match(logsLayout, /id="f_name_dropdown" class="filter-dropdown"/);
  assert.match(logsLayout, /id="f_model" class="filter-select filter-combobox"/);
  assert.match(logsLayout, /id="f_model_dropdown" class="filter-dropdown"/);
  assert.match(logsLayout, /id="f_status" class="filter-input filter-control--narrow"/);
  assert.match(logsLayout, /class="filter-group logs-filter-group logs-filter-group--token"[\s\S]*id="f_auth_token" class="filter-select filter-control--wide logs-filter-control--token"/);
  assert.match(trendLayout, /id="f_model" class="filter-select filter-control--wide"/);
  assert.match(trendLayout, /class="filter-controls trend-filter-controls"/);
  assert.doesNotMatch(statsLayout, /logs-filter-control--(?:range|channel-id|token)/);
  assert.doesNotMatch(trendLayout, /logs-filter-control--(?:range|channel-id|token)/);
});

test('logs.html、stats.html 和 trend.html 通过占位节点接入共享筛选栏，并在页面脚本前加载 page-filters', () => {
  assert.match(logsHtml, /data-page-filters="logs"/);
  assert.match(statsHtml, /data-page-filters="stats"/);
  assert.match(trendHtml, /data-page-filters="trend"/);

  assert.match(logsHtml, /page-filters\.js\?v=__VERSION__[\s\S]*logs\.js\?v=__VERSION__/);
  assert.match(statsHtml, /page-filters\.js\?v=__VERSION__[\s\S]*stats\.js\?v=__VERSION__/);
  assert.match(trendHtml, /page-filters\.js\?v=__VERSION__[\s\S]*trend\.js\?v=__VERSION__/);
});
