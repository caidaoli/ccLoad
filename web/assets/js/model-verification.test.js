const test = require('node:test');
const assert = require('node:assert/strict');

const { summarizeModelVerification } = require('./model-verification.js');

function translate(_key, fallback, params) {
  let text = fallback;
  Object.entries(params || {}).forEach(([name, value]) => {
    text = text.replace(new RegExp(`\\{${name}\\}`, 'g'), String(value));
  });
  return text;
}

test('summarizeModelVerification labels a response model mismatch without claiming proof', () => {
  const summary = summarizeModelVerification({
    claimed_model: 'gpt-5.5',
    effective_model: 'gpt-5.5',
    reported_model: 'gpt-5.3-codex-spark',
    verdict: 'mismatch',
    source: 'unknown',
    catalog: {
      attempted: true,
      available: true,
      model_count: 3,
      effective_model_listed: true,
      reported_model_listed: false
    }
  }, translate);

  assert.equal(summary.className, 'model-verification--mismatch');
  assert.match(summary.label, /模型名不一致/);
  assert.match(summary.title, /响应声明模型：gpt-5.3-codex-spark/);
  assert.match(summary.title, /不能证明底层模型/);
});

test('summarizeModelVerification keeps a matching response explicitly unproven', () => {
  const summary = summarizeModelVerification({
    claimed_model: 'claude-sonnet-4-6',
    effective_model: 'claude-sonnet-4-6',
    reported_model: 'claude-sonnet-4-6',
    verdict: 'consistent',
    source: 'unknown',
    catalog: { attempted: false }
  }, translate);

  assert.equal(summary.className, 'model-verification--consistent');
  assert.match(summary.label, /未证实/);
});

test('summarizeModelVerification surfaces web bridge evidence independently', () => {
  const summary = summarizeModelVerification({
    claimed_model: 'gpt-5.5',
    effective_model: 'gpt-5.5',
    verdict: 'unverified',
    source: 'likely_web_bridge',
    catalog: { attempted: false }
  }, translate);

  assert.equal(summary.className, 'model-verification--unverified model-verification--web-bridge');
  assert.match(summary.label, /疑似网页桥接/);
  assert.match(summary.title, /ChatGPT Web 后端接口特征/);
});
