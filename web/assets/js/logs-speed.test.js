const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}(`;
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

test('日志页流式速度优先按首字后的生成阶段计算 tok/s', () => {
  const calculateTokenSpeed = vm.runInNewContext(
    `(${extractFunction(uiSource, 'calculateTokenSpeed')})`,
    {}
  );
  const calculateLogSpeed = vm.runInNewContext(
    `(${extractFunction(logsSource, 'calculateLogSpeed')})`,
    { calculateTokenSpeed }
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 111,
      duration: 5.9,
      first_byte_time: 4.1
    }),
    61.66666666666664
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: false,
      output_tokens: 736,
      duration: 17.0,
      first_byte_time: 2.9
    }),
    43.294117647058826
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 957,
      duration: 21.0,
      first_byte_time: 3.2
    }),
    53.764044943820224
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 100,
      duration: 3,
      first_byte_time: 3
    }),
    33.333333333333336
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 100,
      duration: 3,
      first_byte_time: 2.2
    }),
    33.333333333333336
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 0,
      duration: 12,
      first_byte_time: 2
    }),
    null
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 437,
      duration: 19.98,
      first_byte_time: 0
    }),
    21.87187187187187
  );
});
