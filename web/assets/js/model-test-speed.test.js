const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

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

test('model-test 页流式速度优先按首字后的生成阶段计算 tok/s', () => {
  const calculateTokenSpeed = vm.runInNewContext(
    `(${extractFunction(uiSource, 'calculateTokenSpeed')})`,
    {}
  );
  const pickPositiveTokenCount = vm.runInNewContext(
    `(${extractFunction(script, 'pickPositiveTokenCount')})`,
    {}
  );
  const calculateTestSpeed = vm.runInNewContext(
    `(${extractFunction(script, 'calculateTestSpeed')})`,
    { calculateTokenSpeed, pickPositiveTokenCount }
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 5900,
      first_byte_duration_ms: 4100
    }, {
      output_tokens: 111
    }),
    61.66666666666664
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 17000
    }, {
      completion_tokens: 736
    }),
    43.294117647058826
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 5780
    }, {
      output_tokens: 0,
      completion_tokens: 357
    }),
    61.76470588235294
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 21000,
      first_byte_duration_ms: 3200
    }, {
      output_tokens: 957
    }),
    53.764044943820224
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 3000,
      first_byte_duration_ms: 3000
    }, {
      candidatesTokenCount: 100
    }),
    33.333333333333336
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 3000,
      first_byte_duration_ms: 2200
    }, {
      candidatesTokenCount: 100
    }),
    33.333333333333336
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 12000
    }, {
      output_tokens: 0
    }),
    null
  );

  assert.equal(
    calculateTestSpeed({
      duration_ms: 19980,
      first_byte_duration_ms: 0
    }, {
      output_tokens: 437
    }),
    21.87187187187187
  );
});

test('model-test 页速度列参与排序', () => {
  const getRowSortValue = vm.runInNewContext(
    `(${extractFunction(script, 'getRowSortValue')})`,
    {
      parseNumericCellValue(text) {
        return Number.parseFloat(String(text));
      }
    }
  );

  const row = {
    children: [],
    querySelector(selector) {
      if (selector === '.speed') {
        return { textContent: '45.6' };
      }
      return null;
    }
  };

  assert.equal(getRowSortValue(row, 'speed'), 45.6);
});
