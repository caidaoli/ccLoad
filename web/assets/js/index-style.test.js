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
