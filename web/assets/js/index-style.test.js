const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');

test('首页保留 hero 标题容器', () => {
  assert.match(html, /class="hero-header\s+animate-slide-up"/);
});

test('首页 hero 标题不再使用顶部装饰线', () => {
  assert.doesNotMatch(css, /\.hero-header::before\s*\{/);
});

test('首页自定义时间弹层使用紧凑字号', () => {
  assert.match(css, /\.custom-range-summary\s*\{[\s\S]*?font-size:\s*14px;/);
  assert.match(css, /\.custom-range-calendar-title\s*\{[\s\S]*?font-size:\s*16px;/);
  assert.match(css, /\.custom-range-weekdays span\s*\{[\s\S]*?font-size:\s*13px;/);
  assert.match(css, /\.custom-range-day\s*\{[\s\S]*?font-family:\s*inherit;[\s\S]*?font-size:\s*14px;/);
  assert.match(css, /\.custom-range-time-row input\s*\{[\s\S]*?font-family:\s*inherit;[\s\S]*?font-size:\s*13px;/);
  assert.match(css, /\.custom-range-link-btn,\s*[\r\n\s]*\.custom-range-confirm-btn\s*\{[\s\S]*?font-family:\s*inherit;[\s\S]*?font-size:\s*13px;/);
});

test('自定义时间弹层未来日期使用禁用态样式', () => {
  assert.match(css, /\.custom-range-day\.disabled,\s*[\r\n\s]*\.custom-range-day:disabled\s*\{[\s\S]*?cursor:\s*not-allowed;/);
  assert.match(css, /\.custom-range-day\.disabled:hover,\s*[\r\n\s]*\.custom-range-day:disabled:hover\s*\{[\s\S]*?background:\s*transparent;/);
});

test('筛选栏自定义时间弹层从筛选控件左侧展开', () => {
  assert.match(css, /\.filter-custom-range-host\s+\.custom-range-picker\s*\{[\s\S]*?left:\s*0;[\s\S]*?right:\s*auto;/);
});

test('筛选栏允许自定义时间弹层覆盖后续内容', () => {
  assert.match(css, /\.filter-bar\s*\{[\s\S]*?position:\s*relative;[\s\S]*?z-index:\s*20;[\s\S]*?overflow:\s*visible;/);
});
