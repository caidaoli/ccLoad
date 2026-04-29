const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'tokens.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const zh = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const en = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

function extractFunctionSource(source, functionName) {
  const start = source.indexOf(`function ${functionName}(`);
  if (start === -1) {
    throw new Error(`未找到函数 ${functionName}`);
  }

  let braceIndex = source.indexOf('{', start);
  if (braceIndex === -1) {
    throw new Error(`函数 ${functionName} 缺少函数体`);
  }

  let depth = 0;
  let quote = '';
  let escaped = false;

  for (let i = braceIndex; i < source.length; i += 1) {
    const char = source[i];

    if (quote) {
      if (escaped) {
        escaped = false;
        continue;
      }
      if (char === '\\') {
        escaped = true;
        continue;
      }
      if (char === quote) {
        quote = '';
      }
      continue;
    }

    if (char === '"' || char === '\'' || char === '`') {
      quote = char;
      continue;
    }

    if (char === '{') {
      depth += 1;
      continue;
    }
    if (char === '}') {
      depth -= 1;
      if (depth === 0) {
        return source.slice(start, i + 1);
      }
    }
  }

  throw new Error(`函数 ${functionName} 提取失败`);
}

function buildTokensChannelRuntime() {
  const functionNames = [
    'normalizeChannelTypeValue',
    'buildChannelTypeDisplayNameMap',
    'ensureChannelTypeDisplayNameMap',
    'getChannelTypeGroupKey',
    'getChannelTypeGroupLabel',
    'matchesChannelSearchText'
  ];
  const context = {
    Map,
    Array,
    String,
    Promise,
    console,
    channelTypeDisplayNameMap: new Map(),
    channelTypeDisplayNamesPromise: null,
    window: {},
    t: (key) => key === 'tokens.channelTypeOther' ? 'Other' : key
  };
  const source = functionNames.map(name => extractFunctionSource(script, name)).join('\n\n');
  vm.createContext(context);
  vm.runInContext(`${source}\nthis.__exports = { ${functionNames.join(', ')} };`, context);
  return { context, ...context.__exports };
}

test('tokens 编辑弹窗新增渠道限制区域并使用 90% 桌面宽度和限制区双列布局', () => {
  assert.match(html, /<div class="modal-content modal-content--wide token-edit-modal">/);
  assert.match(html, /<div class="modal-body token-edit-body token-edit-layout">/);
  assert.match(html, /<div class="token-edit-sidebar">[\s\S]*token-edit-section--basic[\s\S]*token-edit-section--quota[\s\S]*<\/div>/);
  assert.match(html, /<div class="token-edit-main">[\s\S]*token-edit-section--channels[\s\S]*token-edit-section--models[\s\S]*<\/div>/);
  assert.match(html, /data-token-edit-section="channels"/);
  assert.match(html, /id="editAllowedChannelsCount"/);
  assert.match(html, /id="allowedChannelsTableBody"/);
  assert.match(html, /data-action="show-channel-select-modal"/);
  assert.match(html, /data-action="batch-delete-allowed-channels"/);
  assert.match(css, /\.modal-content--wide\s*\{[\s\S]*?width:\s*90%;[\s\S]*?max-width:\s*none;/);
  assert.match(css, /\.token-edit-layout\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*320px minmax\(0,\s*1fr\);/);
  assert.match(css, /\.token-edit-main\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\) minmax\(0,\s*1fr\);/);
  assert.match(css, /\.token-edit-section--channels\s*\{[\s\S]*?flex:\s*1 1 auto;[\s\S]*?min-height:\s*0;/);
  assert.match(css, /\.token-edit-channels-table\s*\{[\s\S]*?flex:\s*1 1 auto;[\s\S]*?min-height:\s*0;[\s\S]*?overflow-y:\s*auto;/);
});

test('tokens 移动端编辑弹窗退化为纵向 B 方案', () => {
  assert.match(css, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.modal-content--wide\s*\{[\s\S]*?width:\s*min\(720px,\s*calc\(100vw - 24px\)\);/);
  assert.match(css, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-layout\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-direction:\s*column;/);
});

test('tokens.js 保存并渲染 allowed_channel_ids', () => {
  assert.match(script, /let editAllowedChannelIDs = \[\];/);
  assert.match(script, /let selectedAllowedChannelIDs = new Set\(\);/);
  assert.match(script, /function renderAllowedChannelsTable\(\)/);
  assert.match(script, /editAllowedChannelIDs = \(token\.allowed_channel_ids \|\| \[\]\)\.slice\(\);/);
  assert.match(script, /allowed_channel_ids:\s*editAllowedChannelIDs,/);
  assert.match(script, /'show-channel-select-modal':\s*\(\)\s*=> showChannelSelectModal\(\)/);
  assert.match(script, /'confirm-channel-selection':\s*\(\)\s*=> confirmChannelSelection\(\)/);
  assert.match(script, /'batch-delete-allowed-channels':\s*\(\)\s*=> batchDeleteSelectedAllowedChannels\(\)/);
  assert.match(script, /'toggle-allowed-channel':\s*\(actionTarget\)\s*=>/);
});

test('tokens 渠道选择弹窗按渠道类型分组并支持下拉筛选分组', () => {
  assert.match(html, /id="channelTypeFilterSelect" class="form-input channel-type-filter-select"[^>]*data-change-action="filter-available-channel-type"/);
  assert.match(script, /function groupChannelsByType\(channels\)/);
  assert.match(script, /function getChannelTypeGroupKey\(channel\)/);
  assert.match(script, /function normalizeChannelTypeValue\(value\)/);
  assert.match(script, /function buildChannelTypeDisplayNameMap\(types\)/);
  assert.match(script, /async function ensureChannelTypeDisplayNameMap\(\)/);
  assert.match(script, /window\.ChannelTypeManager && typeof window\.ChannelTypeManager\.getChannelTypes === 'function'/);
  assert.match(script, /function updateChannelTypeFilterOptions\(channels\)/);
  assert.match(script, /function matchesChannelSearchText\(channel, searchText\)/);
  assert.match(script, /'filter-available-channel-type':\s*\(\)\s*=> filterAvailableChannels\(document\.getElementById\('channelSearchInput'\)\?\.value \|\| ''\)/);
  assert.match(script, /const channelGroups = groupChannelsByType\(channels\);/);
  assert.match(script, /const selectedTypeKey = updateChannelTypeFilterOptions\(availableChannels\);/);
  assert.match(script, /channels = channels\.filter\(ch => getChannelTypeGroupKey\(ch\) === selectedTypeKey\);/);
  assert.match(script, /channels = channels\.filter\(ch => matchesChannelSearchText\(ch, searchText\)\);/);
  assert.match(script, /class="channel-type-group"/);
  assert.doesNotMatch(script, /anthropic:\s*'Claude'/);
  assert.doesNotMatch(script, /gemini:\s*'Gemini'/);
  assert.doesNotMatch(html, /channelTypeQuickSelect/);
  assert.doesNotMatch(script, /toggle-channel-type-group/);
  assert.doesNotMatch(script, /toggleChannelTypeGroup/);
  assert.doesNotMatch(script, /channel-type-group-checkbox/);
  assert.doesNotMatch(script, /tokens\.selectGroupChannels/);
  assert.doesNotMatch(css, /\.channel-type-quick-select/);
  assert.match(css, /\.channel-select-filter-row\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\) 150px;/);
  assert.match(css, /\.channel-type-group-header\s*\{[\s\S]*?display:\s*flex;[\s\S]*?justify-content:\s*space-between;/);
});

test('tokens 渠道类型归一化和搜索匹配与展示名称一致', () => {
  const runtime = buildTokensChannelRuntime();
  const {
    context,
    normalizeChannelTypeValue,
    buildChannelTypeDisplayNameMap,
    getChannelTypeGroupKey,
    getChannelTypeGroupLabel,
    matchesChannelSearchText
  } = runtime;

  assert.equal(normalizeChannelTypeValue('  '), 'anthropic');
  assert.equal(normalizeChannelTypeValue(' Gemini '), 'gemini');
  assert.equal(getChannelTypeGroupKey({ channel_type: '' }), 'anthropic');

  context.channelTypeDisplayNameMap = buildChannelTypeDisplayNameMap([
    { value: 'anthropic', display_name: 'Claude Code' },
    { value: 'gemini', display_name: 'Google Gemini' },
    { value: 'openai', display_name: 'OpenAI' }
  ]);

  assert.equal(getChannelTypeGroupLabel('anthropic'), 'Claude Code');
  assert.equal(getChannelTypeGroupLabel('gemini'), 'Google Gemini');
  assert.equal(getChannelTypeGroupLabel('custom-type'), 'custom-type');

  assert.equal(
    matchesChannelSearchText({ id: 1, name: '默认渠道', channel_type: '' }, 'claude code'),
    true
  );
  assert.equal(
    matchesChannelSearchText({ id: 2, name: 'Gemini 主通道', channel_type: 'gemini' }, 'google gemini'),
    true
  );
  assert.equal(
    matchesChannelSearchText({ id: 3, name: '空类型兼容', channel_type: '' }, 'anthropic'),
    true
  );
  assert.equal(
    matchesChannelSearchText({ id: 4, name: 'OpenAI Main', channel_type: 'openai' }, '不存在的关键字'),
    false
  );
});

test('tokens 渠道类型显示名首次加载失败后可再次重试', async () => {
  const runtime = buildTokensChannelRuntime();
  const { context, ensureChannelTypeDisplayNameMap } = runtime;
  let callCount = 0;

  context.window.ChannelTypeManager = {
    getChannelTypes: async () => {
      callCount += 1;
      if (callCount === 1) {
        throw new Error('network error');
      }
      return [
        { value: 'anthropic', display_name: 'Claude Code' },
        { value: 'gemini', display_name: 'Google Gemini' }
      ];
    }
  };

  await ensureChannelTypeDisplayNameMap();
  assert.equal(callCount, 1);
  assert.equal(context.channelTypeDisplayNamesPromise, null);
  assert.equal(context.channelTypeDisplayNameMap.size, 0);

  const map = await ensureChannelTypeDisplayNameMap();
  assert.equal(callCount, 2);
  assert.equal(context.channelTypeDisplayNamesPromise, null);
  assert.equal(map.get('anthropic'), 'Claude Code');
  assert.equal(map.get('gemini'), 'Google Gemini');
});

test('tokens 模型选择按当前渠道限制聚合可选模型', () => {
  assert.match(script, /function getAvailableModelsForCurrentChannelRestriction\(\)/);
  assert.match(script, /if \(editAllowedChannelIDs\.length === 0\) \{[\s\S]*?return availableModelsCache;/);
  assert.match(script, /const allowedChannelIDs = new Set\(editAllowedChannelIDs\);/);
  assert.match(script, /allChannels\.forEach\(ch => \{[\s\S]*?if \(!allowedChannelIDs\.has\(normalizeChannelID\(ch\.id\)\)\) return;/);
  assert.match(script, /const sourceModels = getAvailableModelsForCurrentChannelRestriction\(\);[\s\S]*?let models = sourceModels\.filter/);
  assert.match(script, /const isEmptyCache = sourceModels\.length === 0;/);
});

test('tokens 渠道限制文案已本地化', () => {
  for (const locale of [zh, en]) {
    assert.match(locale, /'tokens\.channelRestriction':/);
    assert.match(locale, /'tokens\.channelCountSuffix':/);
    assert.match(locale, /'tokens\.selectChannelTitle':/);
    assert.match(locale, /'tokens\.channelTypeAll':/);
    assert.match(locale, /'tokens\.channelTypeFilterTitle':/);
    assert.match(locale, /'tokens\.channelTypeOther':/);
    assert.match(locale, /'tokens\.noChannelRestriction':/);
    assert.match(locale, /'tokens\.msg\.selectAtLeastOneChannel':/);
  }
});
