const test = require('node:test');
const assert = require('node:assert/strict');

const {
  normalizeInlineURLConfigs,
  runtimeInlineURL
} = require('./channels-urls.js');

test('channel URL configs serialize one selected protocol or automatic detection', () => {
  const configs = normalizeInlineURLConfigs([
    {
      url: ' https://upstream.test/v1/messages ',
      exact: true,
      protocols: ['CODEX', 'openai', 'codex']
    },
    {
      url: 'https://automatic.test',
      exact: false,
      protocols: []
    }
  ]);

  assert.deepEqual(configs, [
    {
      url: 'https://upstream.test/v1/messages',
      exact: true,
      protocols: ['openai']
    },
    {
      url: 'https://automatic.test',
      exact: false,
      protocols: []
    }
  ]);
  assert.equal(runtimeInlineURL(configs[0]), 'https://upstream.test/v1/messages#');
  assert.equal(runtimeInlineURL(configs[1]), 'https://automatic.test');
});
