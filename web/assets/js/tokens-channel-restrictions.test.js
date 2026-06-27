const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');

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
