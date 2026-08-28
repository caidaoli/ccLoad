const test = require('node:test');
const assert = require('node:assert/strict');

// 模型重定向显示判定:请求模型与实际模型只是前缀/后缀写法差异时,日志模型列不展示重定向信息。
// 现有 logs-debug-copy.test.js 只覆盖调试弹窗剪贴板行为,模型显示判定另立文件。
const escapeHtmlStub = (str) => String(str);
global.escapeHtml = escapeHtmlStub;
global.window = {
  t: (key) => key,
  escapeHtml: escapeHtmlStub,
  initPageBootstrap() {},
  addEventListener() {},
};
global.document = { addEventListener() {} };
global.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };

const { isPrefixOrSuffixVariant, buildLogModelDisplay } = require('./logs.js');

function hasRedirectMarker(html) {
  return html.includes('redirect-badge') && html.includes('model-redirected');
}

test('相同模型名不算前缀后缀变体判定的差异(无论大小写)', () => {
  assert.equal(isPrefixOrSuffixVariant('gpt-4o', 'gpt-4o'), true);
  assert.equal(isPrefixOrSuffixVariant('GPT-4o', 'gpt-4o'), true);
});

test('仅后缀不同的模型名视为同一模型的写法', () => {
  assert.equal(isPrefixOrSuffixVariant('gemini-3.6-flash-high', 'gemini-3.6-flash'), true);
  assert.equal(isPrefixOrSuffixVariant('gemini-3.6-flash', 'gemini-3.6-flash-high'), true);
});

test('仅前缀不同的模型名视为同一模型的写法', () => {
  assert.equal(isPrefixOrSuffixVariant('deepseek-v4-pro-0813', 'vanchin/deepseek-v4-pro-0813'), true);
  assert.equal(isPrefixOrSuffixVariant('vanchin/deepseek-v4-pro-0813', 'deepseek-v4-pro-0813'), true);
});

test('共享首尾、中间展开的模型名视为同一模型的写法', () => {
  assert.equal(isPrefixOrSuffixVariant('deepseek-v4-pro-0813', 'deepseek-v4-pro-ga-260813'), true);
  assert.equal(isPrefixOrSuffixVariant('deepseek-v4-pro-ga-260813', 'deepseek-v4-pro-0813'), true);
});

test('前缀包含关系按字面规则不视为重定向(gpt-4 vs gpt-4o)', () => {
  assert.equal(isPrefixOrSuffixVariant('gpt-4', 'gpt-4o'), true);
});

test('完全不同或中间段不同的模型名仍视为重定向', () => {
  assert.equal(isPrefixOrSuffixVariant('gpt-4o', 'claude-sonnet-4'), false);
  // 共享首尾但未覆盖短名全部:gemini-2.5 vs gemini-3 是不同模型
  assert.equal(isPrefixOrSuffixVariant('gemini-2.5-flash', 'gemini-3-flash'), false);
});

test('缺少任一模型名时不视为前缀后缀变体', () => {
  assert.equal(isPrefixOrSuffixVariant('', 'gpt-4o'), false);
  assert.equal(isPrefixOrSuffixVariant('gpt-4o', ''), false);
  assert.equal(isPrefixOrSuffixVariant(undefined, 'gpt-4o'), false);
});

test('仅前缀/后缀差异时模型列不渲染重定向信息', () => {
  const html = buildLogModelDisplay('deepseek-v4-pro-0813', 'vanchin/deepseek-v4-pro-0813', '', '');
  assert.equal(hasRedirectMarker(html), false);
  assert.ok(html.includes('deepseek-v4-pro-0813'));
});

test('实质性重定向仍渲染重定向信息与请求/实际模型', () => {
  const html = buildLogModelDisplay('gpt-4o', 'claude-sonnet-4', '', '');
  assert.equal(hasRedirectMarker(html), true);
  assert.ok(html.includes('gpt-4o'));
  assert.ok(html.includes('claude-sonnet-4'));
});

test('无上游声明模型时不渲染重定向信息', () => {
  const html = buildLogModelDisplay('gpt-4o', '', '', '');
  assert.equal(hasRedirectMarker(html), false);
});
