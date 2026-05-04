const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'model-test.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
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

test('model-test 页表头、模板和文案新增速度列', () => {
  assert.match(html, /class="table-col-speed" data-i18n="modelTest\.speed" data-sort-key="speed">速度\(tok\/s\)<\/th>/);
  assert.match(html, /class="model-test-col-speed speed" data-mobile-label="\{\{mobileLabelSpeed\}\}">-<\/td>/);
  assert.match(html, /class="model-test-empty-row"><td colspan="11" data-i18n="modelTest\.selectChannelFirst">请先选择渠道<\/td><\/tr>/);
  assert.match(zhLocale, /'modelTest\.speed': '速度\(tok\/s\)'/);
  assert.match(enLocale, /'modelTest\.speed': 'Speed \(tok\/s\)'/);
});

test('model-test 页速度列使用专用列宽，避免标题挤到缓读列', () => {
  assert.match(script, /<th class="table-col-speed" data-i18n="modelTest\.speed" data-sort-key="speed">速度\(tok\/s\)<\/th>/);
  const styles = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');
  assert.match(styles, /\.table-col-speed\s*\{\s*width:\s*96px;\s*\}/);
});

test('model-test 页流式速度优先按首字后的生成阶段计算 tok/s', () => {
  const calculateTokenSpeed = vm.runInNewContext(
    `(${extractFunction(uiSource, 'calculateTokenSpeed')})`,
    {}
  );
  const calculateTestSpeed = vm.runInNewContext(
    `(${extractFunction(script, 'calculateTestSpeed')})`,
    { calculateTokenSpeed }
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

test('model-test 页速度计算委托给共享 token speed helper', () => {
  assert.match(
    extractFunction(script, 'calculateTestSpeed'),
    /return calculateTokenSpeed\(/
  );
});

test('model-test 页速度列参与排序并使用移动端标签', () => {
  const getResultRowMobileLabels = vm.runInNewContext(
    `(${extractFunction(script, 'getResultRowMobileLabels')})`,
    {
      i18nText(key, fallback) {
        return `${key}:${fallback}`;
      }
    }
  );

  const labels = getResultRowMobileLabels('common.model', '模型');
  assert.equal(labels.mobileLabelSpeed, 'modelTest.speed:速度(tok/s)');

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

test('model-test 页速度单元格只显示数值不重复单位，且保留一位小数', () => {
  assert.doesNotMatch(
    script,
    /speedDisplay[^]*tok\/s/,
    '速度列单元格不应重复拼接 tok/s'
  );
  assert.match(
    script,
    /speedDisplay[^]*testSpeed\.toFixed\(1\)/,
    '速度列应使用一位小数格式渲染'
  );
});
