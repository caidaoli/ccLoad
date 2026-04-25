const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const filterQueryPath = path.join(__dirname, 'filter-query.js');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const trendSource = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');
const logsHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const statsHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const trendHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'trend.html'), 'utf8');

function loadFilterQueryModule() {
  assert.ok(fs.existsSync(filterQueryPath), '缺少共享请求参数模块 filter-query.js');

  const source = fs.readFileSync(filterQueryPath, 'utf8');
  const sandbox = {
    console,
    URLSearchParams,
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return sandbox.window;
}

test('共享请求参数模块暴露 buildRequestParams helper', () => {
  const window = loadFilterQueryModule();

  assert.equal(typeof window.FilterQuery, 'object');
  assert.equal(typeof window.FilterQuery.buildRequestParams, 'function');
});

test('共享请求参数模块支持基础参数、requestKey 和 includeInRequest', () => {
  const window = loadFilterQueryModule();
  const params = window.FilterQuery.buildRequestParams(
    {
      range: 'today',
      authToken: '9',
      channelType: 'all',
      trendType: 'rpm',
      channelName: 'demo'
    },
    [
      { key: 'range', queryKeys: ['range'] },
      { key: 'authToken', queryKeys: ['token'], requestKey: 'auth_token_id' },
      {
        key: 'channelType',
        queryKeys: ['channel_type'],
        includeInRequest(value) {
          return Boolean(value) && value !== 'all';
        }
      },
      {
        key: 'trendType',
        queryKeys: ['type'],
        includeInRequest() {
          return false;
        }
      },
      { key: 'channelName', queryKeys: ['channel_name'] }
    ],
    {
      baseParams: {
        bucket_min: 5
      }
    }
  );

  assert.equal(params.get('bucket_min'), '5');
  assert.equal(params.get('range'), 'today');
  assert.equal(params.get('auth_token_id'), '9');
  assert.equal(params.get('channel_name'), 'demo');
  assert.equal(params.has('channel_type'), false);
  assert.equal(params.has('type'), false);
});

test('共享请求参数模块支持按当前值动态选择 requestKey', () => {
  const window = loadFilterQueryModule();
  const params = window.FilterQuery.buildRequestParams(
    {
      model: 'gpt-5.4',
      modelExact: true,
      channelName: '88',
      channelNameExact: false
    },
    [
      {
        key: 'model',
        queryKeys: ['model', 'model_like'],
        requestKey(value, values) {
          return values.modelExact ? 'model' : 'model_like';
        }
      },
      {
        key: 'channelName',
        queryKeys: ['channel_name', 'channel_name_like'],
        requestKey(value, values) {
          return values.channelNameExact ? 'channel_name' : 'channel_name_like';
        }
      }
    ]
  );

  assert.equal(params.get('model'), 'gpt-5.4');
  assert.equal(params.has('model_like'), false);
  assert.equal(params.get('channel_name_like'), '88');
  assert.equal(params.has('channel_name'), false);
});

test('logs.js、stats.js 和 trend.js 通过共享 helper 构建请求参数', () => {
  assert.match(logsSource, /FilterQuery\.buildRequestParams/);
  assert.match(statsSource, /FilterQuery\.buildRequestParams/);
  assert.match(trendSource, /FilterQuery\.buildRequestParams/);
});

test('logs.js 将 log_source 纳入筛选状态和请求参数定义', () => {
  assert.match(logsSource, /key:\s*'logSource'/);
  assert.match(logsSource, /queryKeys:\s*\['log_source'\]/);
  assert.match(logsSource, /requestKey:\s*'log_source'/);
  assert.match(logsSource, /defaultValue:\s*'proxy'/);
});

test('logs.html、stats.html 和 trend.html 在页面脚本前加载共享请求参数 helper', () => {
  assert.match(
    logsHtmlSource,
    /filter-query\.js\?v=__VERSION__[\s\S]*logs\.js\?v=__VERSION__/
  );
  assert.match(
    statsHtmlSource,
    /filter-query\.js\?v=__VERSION__[\s\S]*stats\.js\?v=__VERSION__/
  );
  assert.match(
    trendHtmlSource,
    /filter-query\.js\?v=__VERSION__[\s\S]*trend\.js\?v=__VERSION__/
  );
});

test('logs.js 的模型与渠道名筛选按选项命中动态选择精确或模糊参数', () => {
  assert.match(logsSource, /function logsFilterMatchesOption\(/);
  assert.match(logsSource, /function rememberExactLogsFilters\(/);
  assert.match(logsSource, /function getLogsChannelNameFilterKey\(/);
  assert.match(logsSource, /function getLogsModelFilterKey\(/);
  assert.match(logsSource, /channelNameExact:\s*isExactLogsChannelNameFilter\(channelName\)/);
  assert.match(logsSource, /modelExact:\s*isExactLogsModelFilter\(model\)/);
  assert.match(logsSource, /key:\s*'channelName',\s*queryKeys:\s*\['channel_name',\s*'channel_name_like'\][\s\S]*?paramKey:\s*getLogsChannelNameFilterKey[\s\S]*?requestKey:\s*getLogsChannelNameFilterKey/);
  assert.match(logsSource, /key:\s*'model',\s*queryKeys:\s*\['model',\s*'model_like'\][\s\S]*?paramKey:\s*getLogsModelFilterKey[\s\S]*?requestKey:\s*getLogsModelFilterKey/);
});

test('stats.js 的模型与渠道名筛选按选项命中动态选择精确或模糊参数', () => {
  assert.match(statsSource, /function statsFilterMatchesOption\(/);
  assert.match(statsSource, /function rememberExactStatsFilters\(/);
  assert.match(statsSource, /function getStatsChannelNameFilterKey\(/);
  assert.match(statsSource, /function getStatsModelFilterKey\(/);
  assert.match(statsSource, /channelNameExact:\s*isExactStatsChannelNameFilter\(channelName\)/);
  assert.match(statsSource, /modelExact:\s*isExactStatsModelFilter\(model\)/);
  assert.match(statsSource, /key:\s*'channelName',\s*queryKeys:\s*\['channel_name',\s*'channel_name_like'\][\s\S]*?paramKey:\s*getStatsChannelNameFilterKey[\s\S]*?requestKey:\s*getStatsChannelNameFilterKey/);
  assert.match(statsSource, /key:\s*'model',\s*queryKeys:\s*\['model',\s*'model_like'\][\s\S]*?paramKey:\s*getStatsModelFilterKey[\s\S]*?requestKey:\s*getStatsModelFilterKey/);
});
