const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const logsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const statsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const trendHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'trend.html'), 'utf8');

test('logs.html、stats.html 和 trend.html 通过占位节点接入共享筛选栏，并在页面脚本前加载 page-filters', () => {
  assert.match(logsHtml, /data-page-filters="logs"/);
  assert.match(statsHtml, /data-page-filters="stats"/);
  assert.match(trendHtml, /data-page-filters="trend"/);

  assert.match(logsHtml, /page-filters\.js\?v=__VERSION__[\s\S]*logs\.js\?v=__VERSION__/);
  assert.match(statsHtml, /page-filters\.js\?v=__VERSION__[\s\S]*stats\.js\?v=__VERSION__/);
  assert.match(trendHtml, /page-filters\.js\?v=__VERSION__[\s\S]*trend\.js\?v=__VERSION__/);
});
