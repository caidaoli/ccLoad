const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const zhLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  const start = source.indexOf(signature);
  assert.ok(start >= 0, `缺少函数 ${name}`);

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

test('stats 页默认排序提示文案描述渠道类型、优先级、名称顺序', () => {
  assert.match(html, /data-i18n="stats\.sortByPriority"/);
  assert.match(zhLocale, /'stats\.sortByPriority': '按渠道类型、优先级、名称排序'/);
  assert.match(enLocale, /'stats\.sortByPriority': 'Sorted by channel type, priority, and name'/);
});

test('stats 页默认按渠道类型、优先级、名称、模型排序', () => {
  const context = {
    statsData: {
      stats: [
        {
          channel_type: 'openai',
          channel_priority: 100,
          channel_name: 'zulu',
          model: 'gpt-4.1'
        },
        {
          channel_type: 'anthropic',
          channel_priority: 1,
          channel_name: 'alpha',
          model: 'claude-3-7-sonnet'
        },
        {
          channel_type: 'openai',
          channel_priority: 200,
          channel_name: 'alpha',
          model: 'gpt-4o'
        },
        {
          channel_type: 'openai',
          channel_priority: 200,
          channel_name: 'alpha',
          model: 'gpt-4.1'
        }
      ]
    },
    sortState: {
      column: null,
      order: null
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'applySorting')}
    ${extractFunction(script, 'applyDefaultSorting')}
  `, context);

  context.applyDefaultSorting();
  assert.deepEqual(
    context.statsData.stats.map((entry) => ({
      channel_type: entry.channel_type,
      channel_priority: entry.channel_priority,
      channel_name: entry.channel_name,
      model: entry.model
    })),
    [
      {
        channel_type: 'anthropic',
        channel_priority: 1,
        channel_name: 'alpha',
        model: 'claude-3-7-sonnet'
      },
      {
        channel_type: 'openai',
        channel_priority: 200,
        channel_name: 'alpha',
        model: 'gpt-4.1'
      },
      {
        channel_type: 'openai',
        channel_priority: 200,
        channel_name: 'alpha',
        model: 'gpt-4o'
      },
      {
        channel_type: 'openai',
        channel_priority: 100,
        channel_name: 'zulu',
        model: 'gpt-4.1'
      }
    ]
  );
});
