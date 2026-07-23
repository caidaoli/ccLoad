const test = require('node:test');
const assert = require('node:assert/strict');

const mod = require('./channels-cooldown-detection.js');

test('cloneRules 按优先级排序、重编号且不共享输入', () => {
  const source = {
    rules: [
      { enabled: true, name: 'Maintenance', priority: 9, status_codes: [503], scope: 'channel', mode: 'fixed', cooldown_seconds: 120 },
      { enabled: false, name: 'Rate limit', priority: 3, status_codes: [429], scope: 'key', mode: 'fixed', cooldown_seconds: 60 }
    ]
  };

  const copy = mod.cloneRules(source);
  assert.deepEqual(copy.rules.map((rule) => rule.priority), [0, 1]);
  assert.equal(copy.rules[0].status_codes, '429');
  assert.equal(copy.rules[0].enabled, false);
  assert.equal(copy.rules[1].name, 'Maintenance');
  copy.rules[0].scope = 'model';
  assert.equal(source.rules[1].scope, 'key');
});

test('提交载荷用显示顺序作为唯一优先级', () => {
  mod.resetCooldownDetectionState({
    rules: [
      { enabled: true, name: 'Maintenance', priority: 8, status_codes: [503, 429], code_pattern: 'legacy-only', message_pattern: 'maintenance', scope: 'channel', mode: 'fixed', cooldown_seconds: 120 },
      { enabled: true, name: '  Retry later  ', priority: 1, message_pattern: 'retry', scope: 'model', mode: 'fixed', cooldown_seconds: 30 }
    ]
  });

  const payload = mod.collectCooldownDetectionRulesForSubmit();
  assert.ok(payload);
  assert.deepEqual(payload.rules.map((rule) => rule.priority), [0, 1]);
  assert.equal(payload.rules[0].scope, 'model');
  assert.equal(payload.rules[0].name, 'Retry later');
  assert.deepEqual(payload.rules[1].status_codes, [503, 429]);
  assert.equal(payload.rules[1].message_pattern, 'maintenance');
  assert.equal(Object.hasOwn(payload.rules[1], 'code_pattern'), false);
});

test('本地校验阻止无条件规则、重复状态码和无效动态字段', () => {
  const errors = mod.validateCooldownDetectionRulesLocally({
    rules: [
      { enabled: true, name: 'Broken fixed', priority: 0, status_codes: '429, 429', scope: 'channel', mode: 'fixed', cooldown_seconds: 0 },
      { enabled: true, name: 'Broken dynamic', priority: 1, message_pattern: '', scope: 'key', mode: 'reset_time', time_capture: 'bad-name', time_layout: '', timezone: '' }
    ]
  });

  assert.ok(errors.length >= 6, `expected several validation errors, got ${errors}`);
});

test('动态规则按时间类型提交，非日期时间不携带布局和时区', () => {
  mod.resetCooldownDetectionState({
    rules: [{
      enabled: true,
      name: 'Retry seconds',
      priority: 0,
      status_codes: [200],
      message_pattern: 'retry in (?P<seconds>\\d+)',
      scope: 'key',
      mode: 'reset_time',
      time_capture: 'seconds',
      time_format: 'duration_seconds',
      time_layout: '2006-01-02 15:04:05',
      timezone: 'UTC'
    }]
  });

  const payload = mod.collectCooldownDetectionRulesForSubmit();
  assert.equal(payload.rules[0].time_format, 'duration_seconds');
  assert.equal(payload.rules[0].time_capture, 'seconds');
  assert.equal(Object.hasOwn(payload.rules[0], 'time_layout'), false);
  assert.equal(Object.hasOwn(payload.rules[0], 'timezone'), false);
});

test('Unix 和剩余秒数不要求布局和时区', () => {
  const errors = mod.validateCooldownDetectionRulesLocally({
    rules: [
      {
        enabled: true, priority: 0, status_codes: '200', message_pattern: '(?P<until>\\d+)',
        name: 'Unix reset',
        scope: 'key', mode: 'reset_time', time_capture: 'until', time_format: 'unix'
      },
      {
        enabled: true, priority: 1, status_codes: '429', message_pattern: '(?P<seconds>\\d+)',
        name: 'Duration reset',
        scope: 'channel', mode: 'reset_time', time_capture: 'seconds', time_format: 'duration_seconds'
      }
    ]
  });

  assert.deepEqual(errors, []);
});

test('提交前校验拒绝空规则名称', () => {
  mod.resetCooldownDetectionState({
    rules: [{
      enabled: true,
      priority: 0,
      status_codes: [200],
      scope: 'key',
      mode: 'fixed',
      cooldown_seconds: 60
    }]
  });

  const errors = mod.validateCooldownDetectionRulesForSubmit();
  assert.equal(errors.length, 1);
  assert.match(errors[0], /Rule name is required/);
});

test('空规则集提交为 null', () => {
  mod.resetCooldownDetectionState(null);
  assert.equal(mod.collectCooldownDetectionRulesForSubmit(), null);
});
