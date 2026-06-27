const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const sseMergeSource = fs.readFileSync(path.join(__dirname, 'sse-merge.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}(`;
  const start = source.indexOf(signature);
  assert.notEqual(start, -1, `缺少函数 ${name}`);

  const bodyStart = source.indexOf('{', start);
  assert.notEqual(bodyStart, -1, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = bodyStart; i < source.length; i++) {
    const ch = source[i];
    if (ch === '{') depth++;
    if (ch === '}') {
      depth--;
      if (depth === 0) {
        return source.slice(start, i + 1);
      }
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}

function createHelpers() {
  const sandbox = { window: {} };
  vm.createContext(sandbox);
  // sse-merge.js 是 appendMergedText / collectMergedResponsePayload / parseSSEDataPayloads 的
  // 规范实现，通过 IIFE 将自身挂到 window.SSEMerge；logs.js 的 composeDebugMergedResponse
  // 已改为调用 window.SSEMerge.* 而非内联副本。
  vm.runInContext(sseMergeSource, sandbox);
  vm.runInContext(`
${extractFunction(logsSource, 'formatJsonSafe')}
${extractFunction(logsSource, 'composeDebugMergedResponse')}
this.__logsDebugMergeTest = {
  composeDebugMergedResponse
};
`, sandbox);

  return sandbox.__logsDebugMergeTest;
}

test('合并 SSE responses 输出文本 delta', () => {
  const helpers = createHelpers();
  const merged = helpers.composeDebugMergedResponse({
    resp_body: [
      'event: response.output_text.delta',
      'data: {"type":"response.output_text.delta","delta":"Release","output_index":1,"content_index":0}',
      '',
      'event: response.output_text.delta',
      'data: {"type":"response.output_text.delta","delta":" 工作流","output_index":1,"content_index":0}',
      '',
      'event: response.completed',
      'data: {"type":"response.completed","response":{"status":"completed"}}'
    ].join('\n')
  });

  assert.equal(merged, 'Release 工作流');
});

test('合并 Gemini SSE 时拼接 candidates content parts text', () => {
  const helpers = createHelpers();
  const merged = helpers.composeDebugMergedResponse({
    resp_body: [
      'HTTP 200',
      'Content-Type: text/event-stream',
      '',
      `data: ${JSON.stringify({
        candidates: [
          {
            content: {
              parts: [{ text: '{"type":"discovery","title":"' }],
              role: 'model'
            },
            index: 0
          }
        ]
      })}`,
      '',
      `data: ${JSON.stringify({
        candidates: [
          {
            content: {
              parts: [{ text: '确认 ProcessManager.ts 文件结束范围","facts":[' }],
              role: 'model'
            },
            index: 0
          }
        ]
      })}`,
      '',
      `data: ${JSON.stringify({
        candidates: [
          {
            content: {
              parts: [{ text: '"560行之后返回空内容"]}' }],
              role: 'model'
            },
            index: 0
          }
        ]
      })}`,
      '',
      `data: ${JSON.stringify({
        candidates: [
          {
            content: {
              parts: [{ text: '', thoughtSignature: 'sig' }],
              role: 'model'
            },
            finishReason: 'STOP',
            index: 0
          }
        ]
      })}`
    ].join('\n')
  });

  assert.equal(merged, '{"type":"discovery","title":"确认 ProcessManager.ts 文件结束范围","facts":["560行之后返回空内容"]}');
});

test('合并 Anthropic SSE 时拼接 thinking 和 text delta', () => {
  const helpers = createHelpers();
  const merged = helpers.composeDebugMergedResponse({
    resp_body: [
      'HTTP 200',
      'Content-Type: text/event-stream',
      '',
      'event: content_block_delta',
      `data: ${JSON.stringify({
        type: 'content_block_delta',
        delta: { type: 'thinking_delta', thinking: '先分析' },
        index: 0
      })}`,
      '',
      'event: content_block_delta',
      `data: ${JSON.stringify({
        type: 'content_block_delta',
        delta: { type: 'thinking_delta', thinking: '原因' },
        index: 0
      })}`,
      '',
      'event: content_block_delta',
      `data: ${JSON.stringify({
        type: 'content_block_delta',
        delta: { type: 'text_delta', text: '{"title"' },
        index: 1
      })}`,
      '',
      'event: content_block_delta',
      `data: ${JSON.stringify({
        type: 'content_block_delta',
        delta: { type: 'text_delta', text: ':"修复合并"}' },
        index: 1
      })}`,
      '',
      'event: message_stop',
      'data: {"type":"message_stop"}'
    ].join('\n')
  });

  assert.equal(merged, '先分析原因\n\n{"title":"修复合并"}');
});

test('合并 SSE responses 时 completed 完整 output 不应重复已拼接的 delta', () => {
  const helpers = createHelpers();
  const merged = helpers.composeDebugMergedResponse({
    resp_body: [
      'event: response.output_text.delta',
      'data: {"type":"response.output_text.delta","delta":"Release","output_index":1,"content_index":0}',
      '',
      'event: response.completed',
      'data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Release"}]}]}}'
    ].join('\n')
  });

  assert.equal(merged, 'Release');
});

test('合并 SSE responses 时多个 function call 参数应分段显示', () => {
  const helpers = createHelpers();
  const merged = helpers.composeDebugMergedResponse({
    resp_body: [
      'data: {"type":"response.function_call_arguments.delta","delta":"{\\"a\\":1}","output_index":2}',
      '',
      'data: {"type":"response.function_call_arguments.delta","delta":"{\\"b\\":2}","output_index":3}'
    ].join('\n')
  });

  assert.equal(merged, '{\"a\":1}\n\n{\"b\":2}');
});

test('合并 SSE responses 时自定义工具调用输入应拼接 delta 且不被 done 重复', () => {
  const helpers = createHelpers();
  const patch = [
    '*** Begin Patch\n',
    '*** Update File: internal/storage/sql/query.go\n',
    '@@\n',
    '-\t\t&c.RPMLimit, &c.ChannelType,\n',
    '+\t\t&c.RPMLimit, &c.MaxConcurrency, &c.ChannelType,\n',
    '*** End Patch\n'
  ].join('');
  const merged = helpers.composeDebugMergedResponse({
    resp_body: [
      'event: response.output_item.added',
      'data: {"type":"response.output_item.added","item":{"id":"ctc_1","type":"custom_tool_call","status":"in_progress","name":"apply_patch"},"output_index":0}',
      '',
      'event: response.custom_tool_call_input.delta',
      `data: ${JSON.stringify({ type: 'response.custom_tool_call_input.delta', delta: patch.slice(0, 35), output_index: 0 })}`,
      '',
      'event: response.custom_tool_call_input.delta',
      `data: ${JSON.stringify({ type: 'response.custom_tool_call_input.delta', delta: patch.slice(35), output_index: 0 })}`,
      '',
      'event: response.custom_tool_call_input.done',
      `data: ${JSON.stringify({ type: 'response.custom_tool_call_input.done', input: patch, output_index: 0 })}`,
      '',
      'event: response.output_item.done',
      `data: ${JSON.stringify({ type: 'response.output_item.done', item: { type: 'custom_tool_call', input: patch }, output_index: 0 })}`
    ].join('\n')
  });

  assert.equal(merged, patch.trim());
});

test('合并普通 chat completion 时抽取 message.content 并按字面输出紧凑 JSON', () => {
  const helpers = createHelpers();
  const content = '{"type":"change","title":"v2.11.5发布构建成功完成"}';
  const merged = helpers.composeDebugMergedResponse({
    resp_body: JSON.stringify({
      choices: [
        {
          message: {
            role: 'assistant',
            content,
            reasoning_content: null
          }
        }
      ]
    })
  });

  assert.equal(merged, content);
  assert.doesNotMatch(merged, /"choices"/);
});

test('合并普通 chat completion 可处理带状态行和响应头的完整原始响应', () => {
  const helpers = createHelpers();
  const merged = helpers.composeDebugMergedResponse({
    resp_body: [
      'HTTP 200',
      'Content-Type: application/json',
      'X-Mife-Upstream-Status: 200',
      '',
      JSON.stringify({
        choices: [
          {
            message: {
              role: 'assistant',
              content: '{"title":"完整原始响应"}'
            }
          }
        ]
      })
    ].join('\n')
  });

  assert.equal(merged, '{"title":"完整原始响应"}');
});

test('合并普通 chat completion 时按字面输出 content，不二次美化', () => {
  const helpers = createHelpers();
  const content = '{\n  "type": "discovery",\n  "facts": ["a", "b"]\n}';
  const merged = helpers.composeDebugMergedResponse({
    resp_body: JSON.stringify({
      choices: [
        {
          message: {
            role: 'assistant',
            content,
            reasoning_content: null
          }
        }
      ]
    })
  });

  assert.equal(merged, content);
});

test('合并普通 chat completion 时保留 reasoning_content 和 content', () => {
  const helpers = createHelpers();
  const merged = helpers.composeDebugMergedResponse({
    resp_body: JSON.stringify({
      choices: [
        {
          message: {
            role: 'assistant',
            content: '{}',
            reasoning_content: 'The user aborted the Docker image build watch process.'
          }
        }
      ]
    })
  });

  assert.match(merged, /^The user aborted/);
  assert.match(merged, /\n\n\{\}/);
});
