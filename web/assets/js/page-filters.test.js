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
  assert.match(html, /class="logs-filter-actions"/);
  assert.match(html, /id="f_channel_type"/);
  assert.match(html, /id="f_hours"/);
  assert.match(html, /id="f_id"/);
  assert.match(html, /id="f_name"/);
  assert.match(html, /id="f_model"/);
  assert.match(html, /id="f_status"/);
  assert.match(html, /id="f_auth_token"/);
  assert.match(html, /id="btn_filter"/);
});

test('page-filters 渲染 stats/trend 布局时保留各自特有控件', () => {
  const pageFilters = loadPageFilters();
  const statsLayout = pageFilters.renderLayout('stats');
  const trendLayout = pageFilters.renderLayout('trend');

  assert.match(statsLayout, /id="f_hide_zero_success"/);
  assert.match(statsLayout, /id="statsCount"/);
  assert.match(trendLayout, /id="f_model" class="filter-select"/);
  assert.match(trendLayout, /data-i18n="trend\.allModels"/);
  assert.doesNotMatch(trendLayout, /id="f_hide_zero_success"/);
});

test('logs.html、stats.html 和 trend.html 通过占位节点接入共享筛选栏，并在页面脚本前加载 page-filters', () => {
  assert.match(logsHtml, /data-page-filters="logs"/);
  assert.match(statsHtml, /data-page-filters="stats"/);
  assert.match(trendHtml, /data-page-filters="trend"/);

  assert.match(logsHtml, /page-filters\.js\?v=__VERSION__[\s\S]*logs\.js\?v=__VERSION__/);
  assert.match(statsHtml, /page-filters\.js\?v=__VERSION__[\s\S]*stats\.js\?v=__VERSION__/);
  assert.match(trendHtml, /page-filters\.js\?v=__VERSION__[\s\S]*trend\.js\?v=__VERSION__/);
});
