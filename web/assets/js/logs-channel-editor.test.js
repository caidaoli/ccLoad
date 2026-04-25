const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const logsScript = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const channelsStateScript = fs.readFileSync(path.join(__dirname, 'channels-state.js'), 'utf8');

function extractFunction(source, name) {
  const pattern = new RegExp(`function ${name}\\([^)]*\\) \\{[\\s\\S]*?\\n\\}`, 'm');
  const match = source.match(pattern);
  assert.ok(match, `缺少函数 ${name}`);
  return match[0];
}

test('日志页接入渠道编辑器桥接脚本', () => {
  assert.match(html, /<script defer src="\/web\/assets\/js\/logs-channel-editor\.js\?v=__VERSION__"><\/script>/);
});

test('日志页渠道列渲染为编辑按钮而不是跳转链接', () => {
  assert.match(logsScript, /function buildChannelTrigger\(channelId,\s*channelName,\s*baseURL = ''\)/);
  assert.match(logsScript, /<button type="button" class="channel-link" data-channel-id="\$\{channelId\}"/);
  assert.match(logsScript, /logChannelClickAction\s*===\s*'navigate'/);
  assert.match(logsScript, /openLogChannelEditor\(channelId\)/);
});

test('日志页渠道按钮点击事件委托到编辑渠道弹窗', () => {
  assert.match(logsScript, /const channelBtn = e\.target\.closest\('\.channel-link\[data-channel-id\]'\);/);
  assert.match(logsScript, /openLogChannelEditor\(channelId\)/);
});

test('日志页脚本与渠道编辑器共享状态脚本不存在重复顶层变量声明', () => {
  const declarationPattern = /^(?:let|const|var)\s+([A-Za-z_$][\w$]*)/gm;
  const logsDeclarations = new Set();
  const sharedDeclarations = new Set();

  let match;
  while ((match = declarationPattern.exec(logsScript))) {
    logsDeclarations.add(match[1]);
  }

  while ((match = declarationPattern.exec(channelsStateScript))) {
    sharedDeclarations.add(match[1]);
  }

  const duplicates = [...logsDeclarations].filter((name) => sharedDeclarations.has(name));
  assert.deepEqual(duplicates, []);
});

test('日志页初始化渠道编辑器时会绑定弹窗静态动作', () => {
  const logsChannelEditorScript = fs.readFileSync(path.join(__dirname, 'logs-channel-editor.js'), 'utf8');

  assert.match(logsChannelEditorScript, /typeof initChannelEditorActions === 'function'/);
  assert.match(logsChannelEditorScript, /initChannelEditorActions\(\);/);
});

test('日志页动态加载渠道编辑器会注入自定义规则弹窗及脚本', () => {
  const logsChannelEditorScript = fs.readFileSync(path.join(__dirname, 'logs-channel-editor.js'), 'utf8');

  assert.match(logsChannelEditorScript, /'customRulesModal'/);
  assert.match(logsChannelEditorScript, /\/web\/assets\/js\/channels-custom-rules\.js/);
});

test('ESC 键优先关闭自定义规则弹窗而不是编辑渠道弹窗', () => {
  const logsChannelEditorScript = fs.readFileSync(path.join(__dirname, 'logs-channel-editor.js'), 'utf8');
  const channelsInitScript = fs.readFileSync(path.join(__dirname, 'channels-init.js'), 'utf8');

  for (const source of [logsChannelEditorScript, channelsInitScript]) {
    const customRulesIdx = source.indexOf('closeCustomRulesModal()');
    const channelModalIdx = source.indexOf('closeModal()');
    assert.ok(customRulesIdx > 0, '缺少 closeCustomRulesModal 调用');
    assert.ok(channelModalIdx > 0, '缺少 closeModal 调用');
    assert.ok(customRulesIdx < channelModalIdx, 'ESC 判断顺序必须 customRulesModal 早于 channelModal');
  }
});

test('日志页进行中请求列数只基于日志表自身，不受渠道弹窗额外表头影响', () => {
  const logsTable = {
    querySelectorAll(selector) {
      assert.equal(selector, 'thead th');
      return new Array(14).fill({});
    }
  };

  const context = {
    document: {
      getElementById(id) {
        assert.equal(id, 'tbody');
        return {
          closest(selector) {
            assert.equal(selector, 'table');
            return logsTable;
          }
        };
      },
      querySelectorAll(selector) {
        if (selector === 'thead th') {
          return new Array(24).fill({});
        }
        return [];
      }
    }
  };

  const getTableColspan = vm.runInNewContext(
    `(${extractFunction(logsScript, 'getTableColspan')})`,
    context
  );

  assert.equal(getTableColspan(), 14);
});
