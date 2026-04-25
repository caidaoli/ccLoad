const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const channelsDataSource = fs.readFileSync(path.join(__dirname, 'channels-data.js'), 'utf8');
const channelsFiltersSource = fs.readFileSync(path.join(__dirname, 'channels-filters.js'), 'utf8');

function loadChannelsDataHarness(filters) {
  const sandbox = {
    console,
    URLSearchParams,
    filters,
    channelsPageSize: 20,
    channelsCurrentPage: 1
  };
  vm.createContext(sandbox);
  vm.runInContext(channelsDataSource, sandbox);
  return sandbox;
}

test('channels 列表参数对完整选项使用精确渠道名和模型键', () => {
  const { buildChannelsListParams } = loadChannelsDataHarness({
    search: 'gpt-5.4',
    searchExact: true,
    status: 'all',
    model: 'gpt-5.4',
    modelExact: true
  });

  const params = buildChannelsListParams('all');

  assert.equal(params.get('channel_name'), 'gpt-5.4');
  assert.equal(params.has('search'), false);
  assert.equal(params.get('model'), 'gpt-5.4');
  assert.equal(params.has('model_like'), false);
});

test('channels 列表参数对非完整选项使用模糊渠道名和模型键', () => {
  const { buildChannelsListParams } = loadChannelsDataHarness({
    search: 'gpt-5',
    searchExact: false,
    status: 'all',
    model: 'gpt-5',
    modelExact: false
  });

  const params = buildChannelsListParams('all');

  assert.equal(params.get('search'), 'gpt-5');
  assert.equal(params.has('channel_name'), false);
  assert.equal(params.get('model_like'), 'gpt-5');
  assert.equal(params.has('model'), false);
});

test('channels 筛选下拉记录渠道名和模型是否精确命中选项', () => {
  assert.match(channelsFiltersSource, /inputId:\s*'modelFilter'[\s\S]*?allowCustomInput:\s*true/);
  assert.match(channelsFiltersSource, /filters\.modelExact\s*=\s*isExactChannelModelFilter\(value\);/);
  assert.match(channelsFiltersSource, /filters\.searchExact\s*=\s*!isAllToken && isExactChannelNameFilter\(raw\);/);
});
