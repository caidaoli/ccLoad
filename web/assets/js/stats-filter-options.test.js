const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  const functionStart = source.indexOf(signature);
  assert.ok(functionStart >= 0, `缺少函数 ${name}`);
  const asyncPrefixStart = source.lastIndexOf('async ', functionStart);
  const start = asyncPrefixStart >= 0 && source.slice(asyncPrefixStart, functionStart) === 'async '
    ? asyncPrefixStart
    : functionStart;

  const braceStart = source.indexOf('{', start);
  assert.ok(braceStart >= 0, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const char = source[i];
    if (char === '{') depth++;
    if (char === '}') depth--;
    if (depth === 0) {
      return source.slice(start, i + 1);
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}

test('stats 筛选选项刷新不重置当前模型和渠道名选择', async () => {
  const setValueCalls = [];
  const refreshCalls = [];

  const context = {
    console,
    URLSearchParams,
    setValueCalls,
    refreshCalls,
    t(key) {
      return key;
    },
    async fetchDataWithAuth(url) {
      assert.equal(url, '/admin/stats/filter-options?range=last_week');
      return {
        channel_names: ['cliProxy-codex'],
        models: ['gpt-5.5']
      };
    }
  };

  vm.runInNewContext(`
    let statsChannelNameOptions = [];
    let statsModelOptions = [];
    let currentChannelType = 'all';
    const statsChannelNameCombobox = {
      setValue(...args) {
        setValueCalls.push(['channelName', ...args]);
      },
      refresh() {
        refreshCalls.push('channelName');
      }
    };
    const statsModelCombobox = {
      setValue(...args) {
        setValueCalls.push(['model', ...args]);
      },
      refresh() {
        refreshCalls.push('model');
      }
    };
    function getStatsFilters() {
      return {
        range: 'last_week',
        channelName: 'cliProxy-codex',
        model: 'gpt-5.5'
      };
    }
    function appendStatsTimeRangeParams(params, filters) {
      params.set('range', filters.range);
      return params;
    }

    ${extractFunction(statsSource, 'loadStatsFilterOptions')}

    this.runLoadStatsFilterOptions = () => loadStatsFilterOptions(true);
  `, context);

  await context.runLoadStatsFilterOptions();

  assert.deepEqual(setValueCalls, []);
  assert.deepEqual(refreshCalls, ['channelName', 'model']);
});
