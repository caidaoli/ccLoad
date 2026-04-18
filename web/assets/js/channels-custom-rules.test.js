const test = require('node:test');
const assert = require('node:assert/strict');

const mod = require('./channels-custom-rules.js');
const {
  validateRulesLocally,
  collectCustomRulesForSubmit,
  resetCustomRulesState,
  cloneRules,
  getState,
  MAX_RULES
} = mod;

test('cloneRules 深拷贝并规范化字段', () => {
  const rules = {
    headers: [{ action: 'OVERRIDE', name: 'X-Foo', value: 'v' }],
    body: [{ action: 'override', path: 'thinking', value: { type: 'adaptive' } }]
  };
  const copy = cloneRules(rules);
  assert.equal(copy.headers[0].action, 'override');
  assert.equal(copy.body[0].value, '{"type":"adaptive"}');
  // 修改副本不影响源
  copy.headers[0].name = 'Y-Bar';
  assert.equal(rules.headers[0].name, 'X-Foo');
});

test('cloneRules 兼容 null / 非对象输入', () => {
  const empty = cloneRules(null);
  assert.deepEqual(empty, { headers: [], body: [] });
});

test('resetCustomRulesState 接受 null 重置为空', () => {
  resetCustomRulesState({ headers: [{ action: 'override', name: 'X', value: 'v' }], body: [] });
  assert.equal(getState().headers.length, 1);
  resetCustomRulesState(null);
  assert.deepEqual(getState(), { headers: [], body: [] });
});

test('validateRulesLocally 接受合法规则', () => {
  const errors = validateRulesLocally({
    headers: [
      { action: 'override', name: 'X-Api-Version', value: '2025-08-07' },
      { action: 'remove', name: 'User-Agent', value: '' }
    ],
    body: [
      { action: 'override', path: 'thinking.budget_tokens', value: '8192' },
      { action: 'remove', path: 'stop_sequences', value: '' }
    ]
  });
  assert.deepEqual(errors, []);
});

test('validateRulesLocally 拒绝空 header 名', () => {
  const errors = validateRulesLocally({
    headers: [{ action: 'override', name: '   ', value: 'v' }],
    body: []
  });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /#1/);
});

test('validateRulesLocally 拒绝 CRLF 注入', () => {
  const errors = validateRulesLocally({
    headers: [{ action: 'override', name: 'X-Foo\r\nInject', value: 'v' }],
    body: []
  });
  assert.equal(errors.length, 1);
});

test('validateRulesLocally 拒绝认证头改写', () => {
  const errors = validateRulesLocally({
    headers: [
      { action: 'override', name: 'Authorization', value: 'Bearer hijack' },
      { action: 'remove', name: 'x-api-key', value: '' }
    ],
    body: []
  });
  assert.equal(errors.length, 2);
});

test('validateRulesLocally 拒绝非法 body path', () => {
  const errors = validateRulesLocally({
    headers: [],
    body: [{ action: 'override', path: 'messages[0].role', value: '"user"' }]
  });
  assert.equal(errors.length, 1);
});

test('validateRulesLocally 要求 body override 值为合法 JSON', () => {
  const errors = validateRulesLocally({
    headers: [],
    body: [{ action: 'override', path: 'thinking', value: 'not json' }]
  });
  assert.equal(errors.length, 1);
});

test('validateRulesLocally 超过上限报错', () => {
  const headers = Array.from({ length: MAX_RULES + 1 }, (_, i) => ({
    action: 'override', name: `X-${i}`, value: 'v'
  }));
  const errors = validateRulesLocally({ headers, body: [] });
  assert.ok(errors.some((e) => /32/.test(e)));
});

test('validateRulesLocally 空输入不报错', () => {
  assert.deepEqual(validateRulesLocally(null), []);
  assert.deepEqual(validateRulesLocally({}), []);
});

test('collectCustomRulesForSubmit 返回 null 当规则全为空', () => {
  resetCustomRulesState(null);
  assert.equal(collectCustomRulesForSubmit(), null);
});

test('collectCustomRulesForSubmit 过滤掉空 name / 非法 JSON', () => {
  resetCustomRulesState({
    headers: [
      { action: 'override', name: '  ', value: 'v' }, // 空 name → 丢弃
      { action: 'remove', name: 'User-Agent', value: '' }
    ],
    body: [
      { action: 'override', path: 'thinking', value: '{"type":"adaptive"}' },
      { action: 'override', path: 'bad', value: 'not json' }, // 非法 JSON → 丢弃
      { action: 'remove', path: '  ', value: '' } // 空 path → 丢弃
    ]
  });
  const payload = collectCustomRulesForSubmit();
  assert.ok(payload);
  assert.equal(payload.headers.length, 1);
  assert.equal(payload.headers[0].name, 'User-Agent');
  assert.ok(!('value' in payload.headers[0]), 'remove 头不应包含 value');
  assert.equal(payload.body.length, 1);
  assert.deepEqual(payload.body[0].value, { type: 'adaptive' });
});

test('collectCustomRulesForSubmit 保留 override 头值（空字符串也允许）', () => {
  resetCustomRulesState({
    headers: [{ action: 'override', name: 'X-Blank', value: '' }],
    body: []
  });
  const payload = collectCustomRulesForSubmit();
  assert.ok(payload);
  assert.equal(payload.headers[0].value, '');
});
