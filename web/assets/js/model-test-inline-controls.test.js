const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'model-test.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const sharedCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  let start = source.indexOf(`async ${signature}`);
  if (start < 0) {
    start = source.indexOf(signature);
  }
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

function extractCssRules(css, selector) {
  const rules = [];
  let searchFrom = 0;

  while (searchFrom < css.length) {
    const index = css.indexOf(selector, searchFrom);
    if (index < 0) break;

    const braceStart = css.indexOf('{', index);
    const braceEnd = css.indexOf('}', braceStart);
    assert.ok(braceStart >= 0 && braceEnd >= 0, `CSS rule ${selector} 大括号未闭合`);
    rules.push(css.slice(braceStart + 1, braceEnd));
    searchFrom = braceEnd + 1;
  }

  assert.ok(rules.length > 0, `缺少 CSS rule ${selector}`);
  return rules;
}

function createDomElement(tagName, attrs = {}) {
  const element = {
    tagName: String(tagName || '').toUpperCase(),
    children: [],
    parentElement: null,
    attributes: new Map(),
    style: {},
    _textContent: '',
    className: '',
    id: '',
    value: '',
    placeholder: '',
    addEventListener() {},
    appendChild(child) {
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(child, reference) {
      child.parentElement = this;
      const index = this.children.indexOf(reference);
      if (index < 0) {
        this.children.push(child);
      } else {
        this.children.splice(index, 0, child);
      }
      return child;
    },
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
      if (name === 'id') this.id = String(value);
      if (name === 'class') this.className = String(value);
    },
    getAttribute(name) {
      return this.attributes.has(name) ? this.attributes.get(name) : null;
    },
    removeAttribute(name) {
      this.attributes.delete(name);
    },
    querySelector(selector) {
      return queryTree(this, selector)[0] || null;
    },
    querySelectorAll(selector) {
      return queryTree(this, selector);
    }
  };

  Object.defineProperty(element, 'textContent', {
    get() {
      return this._textContent + this.children.map(child => child.textContent).join('');
    },
    set(value) {
      this._textContent = String(value || '');
      this.children = [];
    }
  });

  Object.entries(attrs).forEach(([key, value]) => {
    if (key === 'textContent') {
      element.textContent = value;
      return;
    }
    element.setAttribute(key, value);
  });

  return element;
}

function matchesSelector(element, selector) {
  if (selector === '[data-i18n]') {
    return element.getAttribute('data-i18n') !== null;
  }
  if (selector.startsWith('#')) {
    return element.id === selector.slice(1);
  }
  if (selector.startsWith('.')) {
    return String(element.className || '').split(/\s+/).includes(selector.slice(1));
  }
  const attrMatch = selector.match(/^([a-z]+)\[([^=]+)="([^"]+)"\]$/i);
  if (attrMatch) {
    const [, tag, attr, value] = attrMatch;
    return element.tagName === tag.toUpperCase() && element.getAttribute(attr) === value;
  }
  return element.tagName === selector.toUpperCase();
}

function queryTree(root, selector) {
  const result = [];
  const visit = (element) => {
    if (matchesSelector(element, selector)) {
      result.push(element);
    }
    element.children.forEach(visit);
  };
  root.children.forEach(visit);
  return result;
}

function findElementById(root, id) {
  return queryTree(root, `#${id}`)[0] || null;
}

test('model-test 页静态控件不再使用内联事件', () => {
  assert.doesNotMatch(html, /onclick="(?:setTestMode|fetchAndAddModels|deleteSelectedModels|runModelTests)\([^"]*\)"/);
  assert.doesNotMatch(html, /onchange="toggleAllModels\(this\.checked\)"/);
  assert.match(html, /data-action="set-test-mode"/);
  assert.doesNotMatch(html, /data-action="select-all-models"/);
  assert.doesNotMatch(html, /data-action="deselect-all-models"/);
  assert.match(html, /data-action="fetch-and-add-models"/);
  assert.match(html, /data-action="open-add-models-modal"/);
  assert.match(html, /data-action="delete-selected-models"/);
  assert.match(html, /data-action="run-model-tests"/);
  assert.match(html, /data-change-action="toggle-all-models"/);
});

test('model-test 页在按模型测试表头提供批量添加模型按钮和弹窗', () => {
  assert.match(html, /id="fetchModelsBtn"[\s\S]*?id="addModelsBtn"[\s\S]*?id="deleteModelsBtn"[\s\S]*?id="runTestBtn"/);
  assert.match(html, /<div id="addModelsModal" class="modal">/);
  assert.match(html, /<textarea[\s\S]*id="addModelsTextarea"[\s\S]*data-i18n-placeholder="modelTest\.addModelsPlaceholder"/);
  assert.match(html, /id="addModelsConfirmBtn"[\s\S]*data-i18n="modelTest\.addModelsConfirm"/);
});

test('i18n.js 暴露的 window.i18nText 对动态删除文案执行插值兜底', () => {
  const i18nScript = fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8');
  const sandbox = {
    window: {
      I18N_LOCALES: {
        'zh-CN': {
          'modelTest.deleteSuccessSummary': '删除完成：成功 {success_channels} 个渠道，失败 {failed_channels} 个渠道'
        }
      }
    },
    document: { documentElement: {} },
    navigator: {},
    localStorage: { getItem() { return null; }, setItem() {} },
    console
  };

  vm.runInNewContext(i18nScript, sandbox);

  assert.equal(typeof sandbox.window.i18nText, 'function', 'i18n.js 应当暴露 window.i18nText');
  assert.equal(
    sandbox.window.i18nText(
      'modelTest.deleteSuccessSummary',
      'fallback',
      { success_channels: 2, failed_channels: 0 }
    ),
    '删除完成：成功 2 个渠道，失败 0 个渠道'
  );

  assert.equal(
    sandbox.window.i18nText('missing.key', '回退 {count} 项', { count: 5 }),
    '回退 5 项',
    'i18nText 在 key 缺失时应使用 fallback 并完成插值'
  );
});

test('model-test 页接入日志页同款渠道编辑器桥接并将渠道名渲染为可点击按钮', () => {
  assert.match(html, /<link rel="stylesheet" href="\/web\/assets\/css\/channels\.css\?v=__VERSION__">/);
  assert.match(html, /<script defer src="\/web\/assets\/js\/logs-channel-editor\.js\?v=__VERSION__"><\/script>/);
  assert.match(html, /<button type="button" class="channel-link" data-channel-id="{{channelId}}" title="{{channelName}}">{{channelName}}<\/button>/);
  assert.match(html, /<button type="button" class="channel-link" data-channel-id="{{channelId}}" title="{{model}}">{{displayName}}<\/button>/);
});

test('model-test 页在按模型测试模式下提供类型筛选并保留协议转换容器', () => {
  assert.match(html, /id="modelTypeLabel"/);
  assert.match(html, /id="testModelType"/);
  assert.match(html, /class="model-test-control model-test-control--type hidden"/);
  assert.match(html, /data-i18n="common\.type"/);
  assert.match(html, /id="protocolTransformContainer"/);
  assert.match(html, /id="protocolTransformOptions"/);
  assert.match(html, /data-i18n="modelTest\.protocolTransform"/);
  assert.match(html, /id="modelTypeLabel"[\s\S]*?id="modelSelectorLabel"[\s\S]*?id="protocolTransformContainer"[\s\S]*?id="streamEnabled"[\s\S]*?id="concurrency"[\s\S]*?id="modelTestContent"/);
});

test('model-test 对话面板 hidden 状态必须覆盖 chat-panel 的 display 规则', () => {
  assert.match(sharedCss, /\.chat-panel\.hidden\s*\{[\s\S]*?display:\s*none\s*!important;[\s\S]*?\}/);
});

test('model-test 对话面板提供独立流式开关并随请求发送', () => {
  assert.match(html, /id="chatStreamEnabled"/);
  assert.match(html, /id="chatChannelSelectContainer"[\s\S]*?id="chatModelSelect"[\s\S]*?id="chatStreamEnabled"[\s\S]*?id="chatClearBtn"/);
  assert.match(script, /const chatStreamEnabled = document\.getElementById\('chatStreamEnabled'\)\?\.checked !== false;/);
  assert.match(script, /body:\s*JSON\.stringify\(\{[\s\S]*?model:\s*chatModel,[\s\S]*?stream:\s*chatStreamEnabled,[\s\S]*?messages:\s*chatMessages/);
  assert.doesNotMatch(script, /body:\s*JSON\.stringify\(\{ model: chatModel,\s*stream: true,\s*messages: chatMessages \}\)/);
});

test('model-test 对话输入区提供思考等级和内置搜索开关并随请求发送', () => {
  assert.match(html, /id="chatInput"[\s\S]*?id="chatThinkingLevel"[\s\S]*?id="chatBuiltinSearchToggle"[\s\S]*?id="chatSendBtn"/);
  assert.match(html, /class="chat-thinking-icon"[\s\S]*?<div class="filter-combobox-wrapper chat-thinking-combobox">[\s\S]*?id="chatThinkingLevel"[\s\S]*?class="filter-select filter-combobox chat-thinking-level-input"[\s\S]*?id="chatThinkingLevelDropdown"[\s\S]*?class="filter-dropdown"/);
  assert.doesNotMatch(html, /<select id="chatThinkingLevel"/);
  assert.doesNotMatch(html, /<option value="(?:|none|minimal|low|medium|high)"/);
  assert.match(html, /id="chatBuiltinSearchToggle"[\s\S]*?aria-pressed="false"[\s\S]*?data-i18n-title="modelTest\.chat\.builtinSearch"/);
  assert.match(html, /id="chatBuiltinSearchToggle"[\s\S]*?<circle cx="12" cy="12" r="9"/);
  assert.match(script, /let chatThinkingCombobox = null;/);
  assert.match(script, /let chatThinkingEffort = '';/);
  assert.match(script, /function getChatThinkingOptions\(\)/);
  assert.match(script, /function getChatThinkingLabel\(value\)/);
  assert.match(script, /function getChatThinkingEffort\(\)/);
  assert.match(script, /chatThinkingCombobox = window\.createSearchableCombobox\(\{[\s\S]*?attachMode:\s*true,[\s\S]*?inputId:\s*'chatThinkingLevel',[\s\S]*?dropdownId:\s*'chatThinkingLevelDropdown',[\s\S]*?allowCustomInput:\s*false,[\s\S]*?getOptions:\s*getChatThinkingOptions,[\s\S]*?onSelect:\s*\(value\)\s*=>\s*\{[\s\S]*?chatThinkingEffort = String\(value \|\| ''\)\.trim\(\);/);
  assert.match(script, /function isChatBuiltinSearchEnabled\(\)/);
  assert.match(script, /'toggle-chat-builtin-search':\s*\(\)\s*=> toggleChatBuiltinSearch\(\)/);
  assert.match(script, /thinking_effort:\s*chatThinkingEffort/);
  assert.match(script, /builtin_search:\s*chatBuiltinSearch/);
  assert.match(sharedCss, /\.chat-input-tools\s*\{/);
  assert.match(sharedCss, /\.chat-thinking-icon\s*\{/);
  assert.match(sharedCss, /\.chat-thinking-combobox\s*\{/);
  assert.match(sharedCss, /\.chat-thinking-level-input\s*\{/);
  assert.match(sharedCss, /\.chat-tool-toggle--search\s*\{/);
  assert.match(sharedCss, /\.chat-tool-toggle\[aria-pressed="true"\]\s*\{/);
});

test('model-test 思考等级 combobox 使用枚举值而不是显示文本', () => {
  const sandbox = {
    i18nText: (_key, fallback) => fallback
  };

  vm.runInNewContext(`
    let chatThinkingEffort = 'high';
    ${extractFunction(script, 'getChatThinkingOptions')}
    ${extractFunction(script, 'getChatThinkingLabel')}
    ${extractFunction(script, 'getChatThinkingEffort')}

    globalThis.result = {
      values: getChatThinkingOptions().map(option => option.value),
      highLabel: getChatThinkingLabel('high'),
      fallbackLabel: getChatThinkingLabel('unexpected'),
      effort: getChatThinkingEffort()
    };
  `, sandbox);

  assert.deepEqual(Array.from(sandbox.result.values), ['', 'none', 'minimal', 'low', 'medium', 'high']);
  assert.equal(sandbox.result.highLabel, '高');
  assert.equal(sandbox.result.fallbackLabel, '默认');
  assert.equal(sandbox.result.effort, 'high');
});

test('model-test 对话输入区支持上传和粘贴图片并发送多模态内容块', () => {
  assert.match(html, /id="chatImagePreviewList"/);
  assert.match(html, /id="chatImageInput"[\s\S]*type="file"[\s\S]*accept="image\/\*"[\s\S]*multiple/);
  assert.match(html, /id="chatImageUploadBtn"[\s\S]*data-action="select-chat-image"[\s\S]*data-i18n-title="modelTest\.chat\.uploadImage"/);
  assert.match(script, /let chatPendingImages = \[\];/);
  assert.match(script, /function handleChatPaste\(event\)/);
  assert.match(script, /function addChatImageFiles\(files\)/);
  assert.match(script, /function buildChatUserContent\(text,\s*images\)/);
  assert.match(script, /function renderChatImagePreviews\(\)/);
  assert.match(script, /function renderChatUserContent\(target,\s*content\)/);
  assert.match(script, /'select-chat-image':\s*\(\)\s*=> document\.getElementById\('chatImageInput'\)\?\.click\(\)/);
  assert.match(script, /'remove-chat-image':\s*\(actionTarget\)\s*=> removeChatImage\(actionTarget\.dataset\.imageId\)/);
  assert.match(script, /'add-chat-images':\s*\(actionTarget\)\s*=> addChatImageFiles\(actionTarget\.files\)/);
  assert.match(script, /chatInput\.addEventListener\('paste',\s*handleChatPaste\)/);
  assert.match(script, /const userContent = buildChatUserContent\(content,\s*chatPendingImages\)/);
  assert.match(script, /chatMessages\.push\(\{ role: 'user', content: userContent \}\)/);
  assert.match(sharedCss, /\.chat-image-preview-list\s*\{/);
  assert.match(sharedCss, /\.chat-image-upload-btn\s*\{/);
});

test('model-test 构建图片消息时文本保持文本、图片转 image_url 块', () => {
  const sandbox = {};
  vm.runInNewContext(`
    ${extractFunction(script, 'buildChatUserContent')}
    globalThis.result = {
      textOnly: buildChatUserContent('hello', []),
      imageOnly: buildChatUserContent('', [{ id: 'img1', dataUrl: 'data:image/png;base64,aW1n', mimeType: 'image/png', name: 'p.png' }]),
      mixed: buildChatUserContent('describe', [{ id: 'img1', dataUrl: 'data:image/png;base64,aW1n', mimeType: 'image/png', name: 'p.png' }])
    };
  `, sandbox);

  assert.equal(sandbox.result.textOnly, 'hello');
  assert.deepEqual(
    JSON.parse(JSON.stringify(sandbox.result.imageOnly)),
    [
      {
        type: 'image_url',
        image_url: {
          url: 'data:image/png;base64,aW1n'
        }
      }
    ]
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(sandbox.result.mixed)),
    [
      {
        type: 'text',
        text: 'describe'
      },
      {
        type: 'image_url',
        image_url: {
          url: 'data:image/png;base64,aW1n'
        }
      }
    ]
  );
});

test('model-test 对话控件跟随对话标签显示在同一行', () => {
  assert.match(html, /<div class="model-test-mode-header">[\s\S]*?<div class="model-test-tabs">[\s\S]*?id="modeTabChat"[\s\S]*?<\/div>[\s\S]*?<div id="chatToolbar" class="chat-toolbar hidden">[\s\S]*?id="chatChannelSelectContainer"[\s\S]*?id="chatModelSelect"[\s\S]*?id="chatStreamEnabled"[\s\S]*?id="chatClearBtn"[\s\S]*?<\/div>[\s\S]*?<\/div>/);
  assert.match(html, /id="chatClearBtn"[\s\S]*data-i18n="modelTest\.chat\.clear">清空<\/button>/);
  const chatPanelMatch = html.match(/<div id="chatPanel" class="chat-panel hidden">([\s\S]*?)<\/div>\s*<\/section>/);
  assert.ok(chatPanelMatch, '缺少 chatPanel');
  assert.doesNotMatch(chatPanelMatch[1], /id="chatToolbar"/);
  assert.match(script, /const chatToolbar = document\.getElementById\('chatToolbar'\);/);
  assert.match(script, /chatToolbar\?\.classList\.toggle\('hidden',\s*!isChatMode\)/);
  const modeHeaderRules = extractCssRules(sharedCss, '.model-test-mode-header');
  assert.ok(
    modeHeaderRules.some(rule => /display:\s*flex;/.test(rule) && /align-items:\s*center;/.test(rule)),
    'model-test-mode-header 必须横向承载 tabs 和对话控件'
  );

  const chatToolbarRules = extractCssRules(sharedCss, '.model-test-mode-header .chat-toolbar');
  assert.ok(
    chatToolbarRules.some(rule =>
      /margin-left:\s*0;/.test(rule) &&
      /margin-bottom:\s*0;/.test(rule) &&
      /justify-content:\s*flex-start;/.test(rule)
    ),
    'chat-toolbar 必须左对齐并跟随 tabs'
  );
  chatToolbarRules.forEach((rule) => {
    assert.doesNotMatch(rule, /margin-left:\s*auto/);
    assert.doesNotMatch(rule, /justify-content:\s*flex-end/);
  });

  const hiddenRules = extractCssRules(sharedCss, '.chat-toolbar.hidden');
  assert.ok(
    hiddenRules.some(rule => /display:\s*none\s*!important;/.test(rule)),
    'chat-toolbar hidden 状态必须强制隐藏'
  );

  const chatControlRules = extractCssRules(sharedCss, '.model-test-mode-header .chat-control');
  assert.ok(
    chatControlRules.some(rule => /flex-shrink:\s*0;/.test(rule) && /min-width:\s*max-content;/.test(rule)),
    'Firefox 下对话控件必须禁止收缩，空间不足时换行而不是互相遮挡'
  );

  const chatControlLabelRules = extractCssRules(sharedCss, '.chat-control__label');
  assert.ok(
    chatControlLabelRules.some(rule => /flex:\s*0 0 auto;/.test(rule)),
    '对话控件标签必须禁止收缩，避免 Firefox 下文字被下拉框压住'
  );

  const chatChannelControlRules = extractCssRules(sharedCss, '.model-test-mode-header .chat-control:first-child');
  assert.ok(
    chatChannelControlRules.some(rule => /flex:\s*0 0 auto;/.test(rule)),
    '渠道控件必须按内容宽度显示，不能把模型控件推远'
  );
  chatChannelControlRules.forEach((rule) => {
    assert.doesNotMatch(rule, /flex:\s*1 1 220px/);
  });
  assert.match(html, /id="chatChannelSelectContainer" class="chat-channel-combobox"/);
  const chatChannelComboboxRules = extractCssRules(sharedCss, '.chat-channel-combobox');
  assert.ok(
    chatChannelComboboxRules.some(rule => /width:\s*250px;/.test(rule) && /flex:\s*0 0 250px;/.test(rule) && /max-width:\s*250px;/.test(rule)),
    '渠道下拉自身宽度必须固定为 250px，避免 Firefox 下挤压后续控件'
  );
  assert.ok(
    chatChannelComboboxRules.some(rule => /width:\s*100%;/.test(rule) && /flex:\s*1 1 auto;/.test(rule) && /max-width:\s*none;/.test(rule)),
    '移动端渠道下拉必须恢复 100% 宽度'
  );

  const chatModelControlRules = extractCssRules(sharedCss, '.model-test-mode-header .chat-control:nth-child(2)');
  assert.ok(
    chatModelControlRules.some(rule => /flex:\s*0 0 auto;/.test(rule)),
    '模型控件必须按内容宽度显示，不能把输入框拉到遮挡标签'
  );
  chatModelControlRules.forEach((rule) => {
    assert.doesNotMatch(rule, /flex:\s*1 1 360px/);
  });

  const chatModelComboboxRules = extractCssRules(sharedCss, '.chat-model-combobox');
  assert.ok(
    chatModelComboboxRules.some(rule => /width:\s*250px;/.test(rule) && /flex:\s*0 0 250px;/.test(rule) && /max-width:\s*250px;/.test(rule)),
    '模型下拉宽度必须固定为 250px，避免 Firefox 下遮挡标签'
  );
  assert.ok(
    chatModelComboboxRules.some(rule => /width:\s*100%;/.test(rule) && /flex:\s*1 1 auto;/.test(rule) && /max-width:\s*none;/.test(rule)),
    '移动端模型下拉必须恢复 100% 宽度'
  );
});

test('model-test 对话模式最大化使用卡片剩余高度', () => {
  assert.match(script, /const modelTestCard = document\.getElementById\('modelTestCard'\);/);
  assert.match(script, /modelTestCard\?\.classList\.toggle\('model-test-card--chat-mode',\s*isChatMode\)/);
  assert.match(sharedCss, /\.model-test-card--chat-mode\s*\{[\s\S]*?height:\s*calc\(100dvh - var\(--topbar-offset, 0px\) - 32px\);[\s\S]*?max-height:\s*calc\(100dvh - var\(--topbar-offset, 0px\) - 32px\);[\s\S]*?display:\s*flex;[\s\S]*?flex-direction:\s*column;[\s\S]*?\}/);
  extractCssRules(sharedCss, '.model-test-card--chat-mode').forEach((rule) => {
    assert.doesNotMatch(rule, /min-height:/);
  });
  assert.match(sharedCss, /\.chat-panel\s*\{[\s\S]*?flex:\s*1 1 auto;[\s\S]*?min-height:\s*0;[\s\S]*?overflow:\s*hidden;[\s\S]*?\}/);
  assert.match(sharedCss, /\.model-test-card--chat-mode \.chat-panel\s*\{[\s\S]*?flex:\s*1 1 auto;[\s\S]*?\}/);
  assert.match(sharedCss, /\.chat-messages\s*\{[\s\S]*?flex:\s*1 1 auto;[\s\S]*?min-height:\s*0;[\s\S]*?overflow-y:\s*auto;[\s\S]*?\}/);
  assert.match(sharedCss, /\.chat-input-area\s*\{[\s\S]*?flex:\s*0 0 auto;[\s\S]*?\}/);
  assert.doesNotMatch(sharedCss, /\.chat-panel\s*\{[\s\S]*?height:\s*calc\(100vh - 220px\)/);
});

test('model-test 渠道下拉标记停用渠道但不阻止选择', () => {
  assert.match(script, /function formatModelTestChannelOptionLabel\(ch\)/);
  assert.match(script, /function getModelTestChannelOptionClass\(ch\)/);
  assert.match(script, /initialLabel = selectedChannel \? formatModelTestChannelOptionLabel\(selectedChannel\) : ''/);
  assert.match(script, /initialLabel: chatChannel \? formatModelTestChannelOptionLabel\(chatChannel\) : ''/);
  assert.equal([...script.matchAll(/label: formatModelTestChannelOptionLabel\(ch\)/g)].length, 2);
  assert.equal([...script.matchAll(/className: getModelTestChannelOptionClass\(ch\)/g)].length, 2);
  assert.match(sharedCss, /\.filter-dropdown-item--disabled\s*\{/);
  assert.match(sharedCss, /\.filter-dropdown-item--disabled:hover\s*\{/);
  const labelFormatter = extractFunction(script, 'formatModelTestChannelOptionLabel');
  assert.doesNotMatch(labelFormatter, /common\.disabled/);
  assert.doesNotMatch(labelFormatter, /\[已禁用\]/);
  assert.doesNotMatch(script, /disabled:\s*ch\.enabled === false/);
});

test('model-test 对话切换渠道后重置为新渠道首个模型', () => {
  const sandbox = {
    chatChannel: {
      models: [
        { model: 'new-channel-model-a' },
        { model: 'new-channel-model-b' }
      ]
    },
    chatModel: 'old-channel-model',
    setValueCalls: [],
    chatModelCombobox: {
      refreshCalled: 0,
      refresh() {
        this.refreshCalled += 1;
      },
      setValue(value, label) {
        sandbox.setValueCalls.push([value, label]);
      }
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'refreshChatModelOptions')}

    refreshChatModelOptions();
    globalThis.result = {
      chatModel,
      refreshCalled: chatModelCombobox.refreshCalled,
      setValueCalls
    };
  `, sandbox);

  assert.equal(sandbox.result.chatModel, 'new-channel-model-a');
  assert.equal(sandbox.result.refreshCalled, 1);
  assert.deepEqual(sandbox.result.setValueCalls, [['new-channel-model-a', 'new-channel-model-a']]);
});

test('model-test 对话流把思考内容渲染到独立折叠区且不写入历史正文', () => {
  assert.match(script, /let accThinking = '';/);
  assert.match(script, /typeof evt\.thinking_delta === 'string'/);
  assert.match(script, /renderChatThinking\(assistantBubble,\s*accThinking,\s*true\)/);
  assert.match(script, /function renderChatThinking\(bubble,\s*thinking,\s*streaming = false\)/);
  assert.match(script, /className = 'chat-thinking'/);
  assert.match(script, /setAttribute\('data-i18n',\s*'modelTest\.chat\.thinking'\)/);
  assert.match(script, /chatMessages\.push\(\{ role: 'assistant', content: accText \}\)/);
  assert.doesNotMatch(script, /chatMessages\.push\(\{ role: 'assistant', content: accThinking/);
  assert.match(sharedCss, /\.chat-thinking\s*\{/);
  assert.match(sharedCss, /\.chat-thinking-content\s*\{/);
});

test('model-test 对话 Markdown 渲染必须消毒后写入 DOM', () => {
  assert.match(script, /function renderChatMarkdown\(target,\s*markdown,\s*options = \{\}\)/);
  assert.match(script, /function sanitizeChatMarkdownHTML\(html\)/);
  assert.match(script, /const ALLOWED_CHAT_MARKDOWN_TAGS = new Set/);
  assert.match(script, /template\.innerHTML = String\(html \|\| ''\)/);
  assert.match(script, /renderChatMarkdown\(contentEl,\s*accText,\s*\{ cursor: true \}\)/);
  assert.doesNotMatch(script, /innerHTML\s*=\s*window\.marked\.parse/);
});

test('model-test 页按模型测试渠道表在渠道名称后显示可编辑优先级和启用开关', () => {
  assert.match(
    script,
    /const MODEL_MODE_HEAD = `[\s\S]*?data-i18n="modelTest\.channelName"[\s\S]*?data-i18n="channels\.table\.priority" data-sort-key="priority">优先级<\/th>[\s\S]*?data-i18n="channels\.table\.enabled" data-sort-key="enabled">启用<\/th>[\s\S]*?\$\{RESPONSE_HEAD_HTML\}/
  );
  assert.doesNotMatch(
    script.match(/const CHANNEL_MODE_HEAD = `[\s\S]*?`;/)[0],
    /data-sort-key="priority"/
  );
  assert.match(
    html,
    /tpl-channel-row-by-model[\s\S]*?class="channel-link"[\s\S]*?{{channelName}}<\/button>[\s\S]*?<td class="model-test-col-priority channel-priority"[\s\S]*?<input class="ch-priority-input"[\s\S]*?value="{{channelPriority}}"[\s\S]*?<\/td>[\s\S]*?<td class="model-test-col-enabled"[\s\S]*?class="channel-enable-switch {{toggleSwitchClass}}"[\s\S]*?data-enabled="{{channelEnabled}}"[\s\S]*?<\/td>/
  );
  assert.match(
    script,
    /channelPriority:\s*String\(priorityValue\)/
  );
  assert.match(
    script,
    /channelEnabled:\s*String\(isEnabled\)/
  );
  assert.match(
    script,
    /toggleSwitchClass:\s*isEnabled\s*\?\s*'channel-enable-switch--on'\s*:\s*'channel-enable-switch--off'/
  );

  const getResultRowMobileLabels = vm.runInNewContext(
    `(${extractFunction(script, 'getResultRowMobileLabels')})`,
    {
      i18nText(key, fallback) {
        return `${key}:${fallback}`;
      }
    }
  );
  assert.equal(
    getResultRowMobileLabels('modelTest.channel', '渠道').mobileLabelPriority,
    'channels.table.priority:优先级'
  );
  assert.equal(
    getResultRowMobileLabels('modelTest.channel', '渠道').mobileLabelEnabled,
    'channels.table.enabled:启用'
  );

  const getResultTableColspan = vm.runInNewContext(`
    const TEST_MODE_MODEL = 'model';
    const RESULT_TABLE_COLSPAN_WITH_FIRST_BYTE = 11;
    const RESULT_TABLE_COLSPAN_NO_FIRST_BYTE = 10;
    const MODEL_MODE_EXTRA_COLSPAN = 2;
    let testMode = TEST_MODE_MODEL;
    function isFirstByteColumnVisible() { return true; }
    (${extractFunction(script, 'getResultTableColspan')})
  `, {});
  assert.equal(getResultTableColspan(), '13');

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
      if (selector === '.ch-priority-input') {
        return { value: '120' };
      }
      if (selector === '.channel-enable-switch') {
        return { dataset: { enabled: 'true' } };
      }
      return null;
    }
  };
  assert.equal(getRowSortValue(row, 'priority'), 120);
  assert.equal(getRowSortValue(row, 'enabled'), 1);

  const disabledRow = {
    children: [],
    querySelector(selector) {
      if (selector === '.channel-enable-switch') {
        return { dataset: { enabled: 'false' } };
      }
      return null;
    }
  };
  assert.equal(getRowSortValue(disabledRow, 'enabled'), 0);
});

test('model-test.js 使用集中绑定处理页面控件和重渲染表头复选框', () => {
  assert.match(script, /window\.initPageBootstrap\(\{/);
  assert.match(script, /topbarKey:\s*'model-test'/);
  assert.match(script, /function initModelTestActions\(\)/);
  assert.match(script, /window\.initDelegatedActions\(\{/);
  assert.match(script, /boundKey:\s*'modelTestActionsBound'/);
  assert.match(script, /'set-test-mode':\s*\(actionTarget\)\s*=> setTestMode\(actionTarget\.dataset\.mode \|\| ''\)/);
  assert.match(script, /'fetch-and-add-models':\s*\(\)\s*=> fetchAndAddModels\(\)/);
  assert.match(script, /'open-add-models-modal':\s*\(\)\s*=> openAddModelsModal\(\)/);
  assert.match(script, /'delete-selected-models':\s*\(\)\s*=> deleteSelectedModels\(\)/);
  assert.match(script, /'run-model-tests':\s*\(\)\s*=> runModelTests\(\)/);
  assert.match(script, /'toggle-all-models':\s*\(actionTarget\)\s*=> toggleAllModels\(actionTarget\.checked\)/);
  assert.match(script, /data-change-action="toggle-all-models"/);
  assert.doesNotMatch(script, /onchange="toggleAllModels/);
  assert.match(script, /const toolbar = document\.querySelector\('\.model-test-toolbar'\);/);
  assert.match(script, /toolbar\?\.classList\.toggle\('model-test-toolbar--model-mode',\s*isModelMode\)/);
  assert.match(script, /bootstrap\(\);/);
});

test('model-test.js 将表头操作按钮渲染进响应内容列并阻止按钮点击触发表头排序', () => {
  assert.match(script, /const RESPONSE_HEAD_HTML = `[\s\S]*?class="table-col-response model-test-response-head"[\s\S]*?class="model-test-toolbar-section model-test-toolbar-section--actions model-test-head-actions"[\s\S]*?id="fetchModelsBtn"[\s\S]*?id="addModelsBtn"[\s\S]*?id="deleteModelsBtn"[\s\S]*?id="runTestBtn"[\s\S]*?`;/);
  assert.match(script, /const CHANNEL_MODE_HEAD = `[\s\S]*?\$\{RESPONSE_HEAD_HTML\}[\s\S]*?`;/);
  assert.match(script, /const MODEL_MODE_HEAD = `[\s\S]*?\$\{RESPONSE_HEAD_HTML\}[\s\S]*?`;/);
  assert.match(script, /th\.onclick = \(event\) => \{[\s\S]*?closest\('\.model-test-head-actions'\)[\s\S]*?return;/);
});

test('model-test.js 批量添加模型输入支持逗号换行去空和大小写去重', () => {
  const sandbox = {};
  vm.runInNewContext(`
    ${extractFunction(script, 'parseBatchModelInput')}
  `, sandbox);

  assert.deepEqual(
    Array.from(sandbox.parseBatchModelInput(' gpt-4o,gpt-4o-mini\nGPT-4O\n\n claude-3-5-sonnet ')),
    ['gpt-4o', 'gpt-4o-mini', 'claude-3-5-sonnet']
  );
});

test('model-test.js 批量添加模型只收集当前勾选渠道', () => {
  const rows = [
    { dataset: { channelId: '10' }, checkbox: { checked: true }, hidden: false },
    { dataset: { channelId: '20' }, checkbox: { checked: false }, hidden: false },
    { dataset: { channelId: '30' }, checkbox: { checked: true }, hidden: true }
  ];
  rows.forEach((row) => {
    row.querySelector = (selector) => selector === '.row-checkbox' ? row.checkbox : null;
  });

  const sandbox = {
    TEST_MODE_MODEL: 'model',
    testMode: 'model',
    channelsList: [
      { id: 10, models: [] },
      { id: 20, models: [] },
      { id: 30, models: [] }
    ],
    document: {
      querySelectorAll(selector) {
        assert.equal(selector, '#model-test-tbody tr[data-channel-id][data-model]');
        return rows;
      }
    },
    isDataRowVisible(row) {
      return !row.hidden;
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getVisibleChannelTargetsForAdd')}
  `, sandbox);

  assert.deepEqual(
    Array.from(sandbox.getVisibleChannelTargetsForAdd().map(target => target.channelId)),
    [10]
  );
});

test('model-test.js 批量添加模型会对当前渠道表格目标逐个保存并更新本地缓存', async () => {
  const calls = [];
  const sandbox = {
    fetchAPIWithAuth: async (url, options) => {
      calls.push({ url, options });
      return { success: true, total: 3 };
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'buildModelEntriesFromNames')}
    ${extractFunction(script, 'appendModelsToChannelCache')}
    ${extractFunction(script, 'executeAddModelsToChannels')}
  `, sandbox);

  const channels = [
    { id: 10, models: [{ model: 'existing', redirect_model: '' }] },
    { id: 20, models: ['legacy'] }
  ];

  const result = await sandbox.executeAddModelsToChannels(
    ['gpt-4o', 'existing'],
    [
      { channelId: 10, channel: channels[0] },
      { channelId: 20, channel: channels[1] }
    ]
  );

  assert.deepEqual(
    calls.map(call => call.url),
    ['/admin/channels/10/models', '/admin/channels/20/models']
  );
  assert.deepEqual(JSON.parse(calls[0].options.body), {
    models: [
      { model: 'gpt-4o', redirect_model: '' },
      { model: 'existing', redirect_model: '' }
    ]
  });
  assert.equal(result.successCount, 2);
  assert.deepEqual(Array.from(result.failed), []);
  assert.deepEqual(channels[0].models.map(entry => entry.model || entry), ['existing', 'gpt-4o']);
  assert.deepEqual(channels[1].models.map(entry => entry.model || entry), ['legacy', 'gpt-4o', 'existing']);
});

test('model-test.js 移除测试数量进度文案，只保留按钮自身测试中状态', () => {
  assert.doesNotMatch(script, /testingProgress/);
  assert.doesNotMatch(script, /completedProgress/);
  assert.match(script, /runTestBtn\.textContent = i18nText\('modelTest\.testing', '测试中\.\.\.'\)/);
});

test('model-test.js 两个模式下都把渠道按钮点击委托到编辑弹窗', () => {
  assert.match(script, /const channelBtn = event\.target\.closest\('\.channel-link\[data-channel-id\]'\);/);
  assert.match(script, /if \(!channelBtn\) return;/);
  assert.match(script, /openLogChannelEditor\(channelId\)/);
});

test('model-test.js 表头模型过滤输入框不被后续全页翻译清掉', () => {
  const root = createDomElement('div');
  const headRow = createDomElement('tr');
  const nameTh = createDomElement('th', {
    'data-sort-key': 'name',
    'data-i18n': 'common.model',
    textContent: '模型'
  });
  headRow.appendChild(nameTh);
  root.appendChild(headRow);

  const sandbox = {
    TEST_MODE_MODEL: 'model',
    testMode: 'channel',
    nameFilterKeyword: '',
    headRow,
    mobileNameFilterInput: null,
    i18nText(key, fallback) {
      return `${key}:${fallback}`;
    },
    document: {
      createElement(tagName) {
        return createDomElement(tagName);
      },
      getElementById(id) {
        return findElementById(root, id);
      }
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getNameFilterPlaceholder')}
    ${extractFunction(script, 'syncNameFilterInputs')}
    ${extractFunction(script, 'renderNameFilterInHeader')}
  `, sandbox);

  sandbox.renderNameFilterInHeader();
  assert.ok(findElementById(root, 'modelTestNameFilter'), '首次渲染后应存在表头过滤输入框');

  root.querySelectorAll('[data-i18n]').forEach((element) => {
    element.textContent = `translated:${element.getAttribute('data-i18n')}`;
  });

  assert.ok(
    findElementById(root, 'modelTestNameFilter'),
    '渠道编辑器触发 translatePage 后不应删除表头过滤输入框'
  );
});

test('model-test.js 渠道编辑器保存后重新加载渠道并保留测试表格状态', () => {
  assert.match(script, /async function loadChannels\(options = \{\}\)/);
  assert.match(script, /const \{ preserveSelection = false,\s*preserveTableState = false \} = options;/);
  assert.match(script, /if \(preserveSelection && preservedChannelId !== null\)/);
  assert.match(script, /window\.ChannelModalHooks = \{[\s\S]*?afterSave:[\s\S]*?loadChannels\(\{ preserveSelection: true,\s*preserveTableState: true \}\)/);
});

test('model-test.js 渠道搜索下拉在重建时通过 initialValue/initialLabel 保持显示当前渠道', () => {
  assert.match(script, /const initialValue = selectedChannel \? String\(selectedChannel\.id\) : '';/);
  assert.match(script, /const initialLabel = selectedChannel \? formatModelTestChannelOptionLabel\(selectedChannel\) : '';/);
  assert.match(script, /initialValue,\s*\n\s*initialLabel,/);
});

test('model-test.js 在模型模式重渲染时保留渠道勾选状态', () => {
  assert.match(script, /function getRowSelectionKey\(row\)/);
  assert.match(script, /function captureRowSelectionState\(\)/);
  assert.match(script, /function restoreRowSelectionState\(row,\s*selectionState,\s*fallbackChecked = true\)/);
  assert.match(script, /const previousSelectionState = captureRowSelectionState\(\);[\s\S]*?restoreRowSelectionState\(row,\s*previousSelectionState,\s*isEnabled\);/);

  const sandbox = {
    tbody: {
      querySelectorAll() {
        return [
          {
            dataset: { channelId: '1', model: 'gpt-4.1' },
            querySelector(selector) {
              if (selector === '.row-checkbox') return { checked: false };
              return null;
            }
          },
          {
            dataset: { channelId: '2', model: 'gpt-4.1' },
            querySelector(selector) {
              if (selector === '.row-checkbox') return { checked: true };
              return null;
            }
          }
        ];
      }
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getRowSelectionKey')}
    ${extractFunction(script, 'captureRowSelectionState')}
    ${extractFunction(script, 'restoreRowSelectionState')}
  `, sandbox);

  const selectionState = sandbox.captureRowSelectionState();
  const preservedRow = {
    dataset: { channelId: '1', model: 'gpt-4.1' },
    querySelector(selector) {
      if (selector === '.row-checkbox') return this.checkbox;
      return null;
    },
    checkbox: { checked: true }
  };
  sandbox.restoreRowSelectionState(preservedRow, selectionState, true);
  assert.equal(preservedRow.checkbox.checked, false);

  const newRow = {
    dataset: { channelId: '3', model: 'gpt-4.1' },
    querySelector(selector) {
      if (selector === '.row-checkbox') return this.checkbox;
      return null;
    },
    checkbox: { checked: false }
  };
  sandbox.restoreRowSelectionState(newRow, selectionState, true);
  assert.equal(newRow.checkbox.checked, true);
});

test('model-test.js 重载渠道后按行键恢复已有测试结果', () => {
  function createCell(value = '') {
    return {
      textContent: value,
      innerHTML: value,
      title: '',
      dataset: {},
      classList: {
        added: [],
        add(name) { this.added.push(name); },
        remove() {}
      }
    };
  }

  function createRow(channelId, model, responseText) {
    const cells = new Map([
      ['.row-checkbox', { checked: true }],
      ['.first-byte-duration', createCell('1.20s')],
      ['.duration', createCell('2.30s')],
      ['.input-tokens', createCell('10')],
      ['.output-tokens', createCell('5')],
      ['.speed', createCell('4.2')],
      ['.cache-read', createCell('1')],
      ['.cache-create', createCell('2')],
      ['.cost', createCell('$0.001')],
      ['.response', createCell(responseText)]
    ]);
    cells.get('.cost').dataset.sortValue = '0.001';
    cells.get('.response').title = responseText;
    return {
      dataset: { channelId, model },
      style: { background: 'rgba(16, 185, 129, 0.1)', color: '' },
      _upstreamData: { url: 'https://upstream.test' },
      querySelector(selector) {
        return cells.get(selector) || null;
      },
      cells
    };
  }

  let currentRows = [createRow('7', 'gpt-4.1', 'old result')];
  const sandbox = {
    tbody: {
      querySelectorAll(selector) {
        if (selector === 'tr[data-channel-id][data-model]') return currentRows;
        return [];
      }
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getRowSelectionKey')}
    ${extractFunction(script, 'getModelTestResultCellSelectors')}
    ${extractFunction(script, 'captureModelTestTableState')}
    ${extractFunction(script, 'restoreModelTestTableState')}
  `, sandbox);

  const state = sandbox.captureModelTestTableState();
  const newRow = createRow('7', 'gpt-4.1', '-');
  newRow.cells.get('.row-checkbox').checked = false;
  newRow.style.background = '';
  newRow._upstreamData = null;

  sandbox.restoreModelTestTableState(new Map([['7::gpt-4.1', newRow]]), state);

  assert.equal(newRow.cells.get('.row-checkbox').checked, true);
  assert.equal(newRow.cells.get('.response').textContent, 'old result');
  assert.equal(newRow.cells.get('.response').title, 'old result');
  assert.equal(newRow.cells.get('.duration').textContent, '2.30s');
  assert.equal(newRow.cells.get('.cost').dataset.sortValue, '0.001');
  assert.equal(newRow._upstreamData.url, 'https://upstream.test');
  assert.deepEqual(newRow.cells.get('.response').classList.added, ['has-upstream-detail']);
});

test('model-test 页渠道按钮去掉默认按钮边框和底色', () => {
  assert.match(sharedCss, /\.model-test-table\s+\.channel-link\s*\{[\s\S]*?padding:\s*0;[\s\S]*?border:\s*none;[\s\S]*?background:\s*transparent;/);
});

test('按协议测试时模型模式按类型和协议联动模型与渠道', () => {
  const sandbox = {
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    selectedModelType: 'anthropic',
    channelsList: [
      { id: 1, name: 'native-anthropic-a', channel_type: 'anthropic', protocol_transforms: [], priority: 10, models: ['claude-4'] },
      { id: 2, name: 'native-openai', channel_type: 'openai', protocol_transforms: [], priority: 5, models: ['gpt-4.1'] },
      { id: 3, name: 'anthropic-openai-transform', channel_type: 'anthropic', protocol_transforms: ['openai'], priority: 3, models: ['claude-3.7'] }
    ]
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelSupportsProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'isModelSupported')}
    ${extractFunction(script, 'getAllModelsForProtocol')}
    ${extractFunction(script, 'getChannelsSupportingModel')}
  `, sandbox);

  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('openai')), ['claude-3.7', 'claude-4', 'gpt-4.1']);
  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('anthropic')), ['claude-3.7', 'claude-4']);
  assert.deepEqual(
    sandbox.getChannelsSupportingModel('openai', 'claude-3.7').map((channel) => channel.id),
    [3]
  );
  sandbox.selectedModelType = 'openai';
  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('openai')), ['claude-3.7', 'gpt-4.1']);
  assert.deepEqual(
    sandbox.getChannelsSupportingModel('openai', 'gpt-4.1').map((channel) => channel.id),
    [2]
  );
});

test('按模型测试时已选模型会同时显示原生和协议转换支持的渠道', () => {
  const sandbox = {
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    selectedModelType: 'anthropic',
    channelsList: [
      { id: 1, name: 'openai-native', channel_type: 'openai', protocol_transforms: [], priority: 50, models: ['gpt-5.4'] },
      { id: 2, name: 'anthropic-native', channel_type: 'anthropic', protocol_transforms: [], priority: 40, models: ['gpt-5.4'] },
      { id: 3, name: 'codex-anthropic-transform', channel_type: 'codex', protocol_transforms: ['anthropic'], priority: 35, models: ['gpt-5.4'] },
      { id: 4, name: 'openai-anthropic-transform', channel_type: 'openai', protocol_transforms: ['anthropic'], priority: 30, models: ['gpt-5.4'] },
      { id: 5, name: 'openai-other-model', channel_type: 'openai', protocol_transforms: ['anthropic'], priority: 45, models: ['gpt-4.1'] }
    ]
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelSupportsProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'isModelSupported')}
    ${extractFunction(script, 'getChannelsSupportingModel')}
  `, sandbox);

  assert.deepEqual(
    sandbox.getChannelsSupportingModel('anthropic', 'gpt-5.4').map((channel) => channel.id),
    [2, 3, 4]
  );
});

test('切换类型后会回退到该类型下的首个可用模型', () => {
  const sandbox = {
    TEST_MODE_MODEL: 'model',
    TEST_MODE_CHANNEL: 'channel',
    testMode: 'model',
    selectedProtocol: 'openai',
    selectedModelType: 'openai',
    selectedModelName: 'claude-4',
    channelsList: [
      { id: 1, channel_type: 'anthropic', models: ['claude-4'] },
      { id: 2, channel_type: 'openai', models: ['gpt-4.1', 'gpt-4.1-mini'] }
    ],
    modelSelectCombobox: { refreshCalled: 0, refresh() { this.refreshCalled += 1; } },
    lastModelValue: '',
    setModelInputValue(value) { globalThis.lastModelValue = value; },
    getModelInputValue() { return ''; },
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini']
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelSupportsProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'getAvailableChannelTypes')}
    ${extractFunction(script, 'ensureSelectedModelType')}
    ${extractFunction(script, 'getAllModelsForProtocol')}
    ${extractFunction(script, 'populateModelSelector')}
  `, sandbox);

  sandbox.populateModelSelector();
  assert.equal(sandbox.selectedModelName, 'gpt-4.1');
  assert.equal(sandbox.modelSelectCombobox.refreshCalled, 1);
});

test('model-test.js 开始测试时发送 protocol_transform 而不是 channel_type', () => {
  assert.match(script, /const selectedProtocol = protocolTransform;/);
  assert.match(script, /protocol_transform:\s*selectedProtocol/);
  assert.doesNotMatch(script, /body:\s*JSON\.stringify\(\{[\s\S]*channel_type:\s*channelType[\s\S]*\}\)/);
});

test('切换渠道后协议默认回退到渠道原生协议', () => {
  assert.match(script, /selectedProtocol\s*=\s*getChannelType\(selectedChannel\)/);
});

test('按模型测试切换协议转换时不重新渲染渠道表格', () => {
  const handlerMatch = script.match(
    /protocolTransformOptions\?\.addEventListener\('change',\s*\(event\)\s*=>\s*\{[\s\S]*?\}\);/
  );
  assert.ok(handlerMatch, '未定位到协议转换 change 处理器');
  const handler = handlerMatch[0];
  assert.match(handler, /if\s*\(testMode\s*===\s*TEST_MODE_MODEL\)\s*\{\s*return;\s*\}/);
  assert.doesNotMatch(handler, /populateModelSelector\(\)/);
  assert.doesNotMatch(handler, /renderModelModeRows\(\)/);
});

test('按模型测试协议切换导致模型输入框失焦时，同模型提交不重新渲染渠道表格', () => {
  const sandbox = {
    window: {
      createSearchableCombobox(config) {
        sandbox.capturedComboboxConfig = config;
        return { setValue() {}, refresh() {} };
      }
    },
    capturedComboboxConfig: null
  };

  vm.runInNewContext(`
    const TEST_MODE_MODEL = 'model';
    let testMode = TEST_MODE_MODEL;
    let selectedModelName = 'claude-4';
    let selectedProtocol = 'anthropic';
    let modelSelectCombobox = null;
    const modelSelect = {};
    let renderCalls = 0;
    function getAllModelsForProtocol() { return ['claude-4']; }
    function getModelInputValue() { return 'claude-4'; }
    function renderModelModeRows() { renderCalls += 1; }
    ${extractFunction(script, 'ensureModelSelectCombobox')}

    ensureModelSelectCombobox();
    capturedComboboxConfig.onSelect('claude-4');
    globalThis.result = { renderCalls, selectedModelName };
  `, sandbox);

  assert.equal(sandbox.result.selectedModelName, 'claude-4');
  assert.equal(sandbox.result.renderCalls, 0);
});

test('按渠道测试时重渲染不会覆盖用户已选的协议转换', () => {
  const sandbox = {
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    TEST_MODE_CHANNEL: 'channel',
    TEST_MODE_MODEL: 'model',
    testMode: 'channel',
    selectedProtocol: 'openai',
    selectedChannel: {
      channel_type: 'anthropic',
      protocol_transforms: ['openai']
    },
    channelsList: []
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'ensureSelectedProtocolForCurrentMode')}
  `, sandbox);

  sandbox.ensureSelectedProtocolForCurrentMode();
  assert.equal(sandbox.selectedProtocol, 'openai');
});

test('按渠道测试时协议选项不再因渠道未配置 protocol_transforms 而禁用', () => {
  const sandbox = {
    TEST_MODE_CHANNEL: 'channel',
    TEST_MODE_MODEL: 'model',
    testMode: 'channel',
    selectedProtocol: 'anthropic',
    selectedChannel: {
      channel_type: 'anthropic',
      protocol_transforms: []
    },
    channelsList: [],
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    protocolTransformOptions: { innerHTML: '' },
    protocolLabel(protocol) {
      return protocol;
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'ensureSelectedProtocolForCurrentMode')}
    ${extractFunction(script, 'renderProtocolTransformOptions')}
  `, sandbox);

  sandbox.renderProtocolTransformOptions();
  assert.doesNotMatch(sandbox.protocolTransformOptions.innerHTML, /\bdisabled\b/);
});

test('applyTestResultToRow 在失败时优先展示结构化上游错误而不是泛化状态文案', () => {
  const cells = new Map([
    ['.first-byte-duration', { textContent: '', title: '' }],
    ['.duration', { textContent: '', title: '' }],
    ['.input-tokens', { textContent: '', title: '' }],
    ['.output-tokens', { textContent: '', title: '' }],
    ['.speed', { textContent: '', title: '' }],
    ['.cache-read', { textContent: '', title: '' }],
    ['.cache-create', { textContent: '', title: '' }],
    ['.cost', { textContent: '', title: '' }],
    ['.response', { textContent: '', title: '' }]
  ]);
  const row = {
    style: {},
    querySelector(selector) {
      const cell = cells.get(selector);
      assert.ok(cell, `缺少单元格 ${selector}`);
      return cell;
    }
  };
  const sandbox = {
    formatDurationMs(value) {
      return value ? `${value}ms` : '-';
    },
    formatCost(value) {
      return String(value);
    },
    i18nText(_key, fallback) {
      return fallback;
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'applyTestResultToRow')}
  `, sandbox);

  sandbox.applyTestResultToRow(row, {
    success: false,
    duration_ms: 1503,
    status_code: 429,
    error: 'API返回错误状态: 429 Too Many Requests',
    api_error: {
      error: '由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试',
      type: 'error'
    }
  });

  assert.equal(
    cells.get('.response').textContent,
    '由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试'
  );
  assert.equal(
    cells.get('.response').title,
    '由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试'
  );
  assert.equal(cells.get('.duration').textContent, '1503ms');
  assert.equal(cells.get('.speed').textContent, '-');
  assert.equal(cells.get('.cost').textContent, '-');
});

test('模型测试遇到 RPM 限制时按秒更新等待提示后重试同一行', async () => {
  const responseHistory = [];
  const responseCell = {
    title: '',
    get textContent() {
      return responseHistory[responseHistory.length - 1] || '';
    },
    set textContent(value) {
      responseHistory.push(String(value || ''));
    }
  };
  const row = {
    style: {},
    querySelector(selector) {
      assert.equal(selector, '.response');
      return responseCell;
    }
  };

  const fetchCalls = [];
  const sleepCalls = [];
  const sandbox = {
    fetchDataWithAuth: async (url, options) => {
      fetchCalls.push({ url, options });
      if (fetchCalls.length === 1) {
        return { success: false, rpm_limited: true, retry_after_ms: 3234, error: '渠道已达到RPM限制' };
      }
      return { success: true, response_text: 'ok' };
    },
    i18nText(_key, fallback, vars) {
      return String(fallback).replace('{seconds}', String(vars?.seconds ?? ''));
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'isRPMLimitedTestResult')}
    ${extractFunction(script, 'getRPMRetryDelayMs')}
    ${extractFunction(script, 'markModelTestRPMWait')}
    ${extractFunction(script, 'sleepModelTest')}
    ${extractFunction(script, 'waitModelTestRPMRetry')}
    ${extractFunction(script, 'fetchModelTestWithRPMWait')}
  `, sandbox);
  sandbox.sleepModelTest = async (delayMs) => {
    sleepCalls.push(delayMs);
  };

  const result = await sandbox.fetchModelTestWithRPMWait(
    { row, channelId: 154 },
    { model: 'qwen3-vl-plus', stream: false, content: 'hi', protocol_transform: 'anthropic' }
  );

  assert.equal(result.success, true);
  assert.equal(fetchCalls.length, 2);
  assert.equal(fetchCalls[0].url, '/admin/channels/154/test');
  assert.equal(fetchCalls[1].url, '/admin/channels/154/test');
  assert.deepEqual(sleepCalls, [1000, 1000, 1000, 234]);
  assert.ok(responseHistory.includes('RPM限制，等待 4s 后重试'));
  assert.ok(responseHistory.includes('RPM限制，等待 3s 后重试'));
  assert.ok(responseHistory.includes('RPM限制，等待 2s 后重试'));
  assert.ok(responseHistory.includes('RPM限制，等待 1s 后重试'));
  assert.equal(responseHistory[responseHistory.length - 1], '测试中...');
});
