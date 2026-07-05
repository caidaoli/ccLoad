const test = require('node:test');
const assert = require('node:assert/strict');

function loadRenderer() {
  function classListFor(owner) {
    const values = new Set();
    return {
      add: (...items) => items.forEach(item => values.add(item)),
      remove: (...items) => items.forEach(item => values.delete(item)),
      contains: (item) => values.has(item),
      toString: () => Array.from(values).join(' '),
      _set(value) {
        values.clear();
        String(value || '').split(/\s+/).filter(Boolean).forEach(item => values.add(item));
        owner._className = Array.from(values).join(' ');
      }
    };
  }

  const matchesSelector = (node, selector) => {
    if (!node) return false;
    if (selector.startsWith('.')) return node.classList?.contains(selector.slice(1));
    if (selector === 'pre code') return node.tagName === 'CODE' && node.parentNode?.tagName === 'PRE';
    if (selector === 'pre > code') return node.tagName === 'CODE' && node.parentNode?.tagName === 'PRE';
    return node.tagName === selector.toUpperCase();
  };

  const element = (tag = 'div') => {
    const el = {
      tagName: tag.toUpperCase(),
      children: [],
      parentNode: null,
      attributes: {},
      _html: '',
      _text: '',
      _className: '',
      set className(value) {
        this.classList._set(value);
      },
      get className() {
        return this.classList.toString();
      },
      set innerHTML(value) { this._html = String(value || ''); },
      get innerHTML() { return this._html; },
      set textContent(value) { this._text = String(value || ''); },
      get textContent() { return this._text; },
      setAttribute(name, value) {
        this.attributes[name] = String(value);
        if (name === 'class') this.className = value;
      },
      getAttribute(name) {
        return this.attributes[name] || '';
      },
      removeAttribute(name) {
        delete this.attributes[name];
      },
      appendChild(child) {
        child.parentNode = this;
        this.children.push(child);
        return child;
      },
      insertBefore(child, ref) {
        child.parentNode = this;
        const index = this.children.indexOf(ref);
        if (index === -1) this.children.push(child);
        else this.children.splice(index, 0, child);
        return child;
      },
      replaceChildren(...children) {
        this.children.forEach(child => { child.parentNode = null; });
        this.children = [];
        children.forEach(child => this.appendChild(child));
      },
      querySelector(selector) {
        return this.querySelectorAll(selector)[0] || null;
      },
      querySelectorAll(selector) {
        const found = [];
        const visit = (node) => {
          node.children.forEach(child => {
            if (matchesSelector(child, selector)) found.push(child);
            visit(child);
          });
        };
        visit(this);
        return found;
      },
      closest(selector) {
        let node = this;
        while (node) {
          if (matchesSelector(node, selector)) return node;
          node = node.parentNode;
        }
        return null;
      }
    };
    el.classList = classListFor(el);
    return el;
  };

  const templateElement = () => ({
    _html: '',
    set innerHTML(value) { this._html = String(value || ''); },
    get innerHTML() { return this._html; },
    content: { querySelectorAll: () => [] }
  });

  global.window = {
    location: { origin: 'http://localhost' },
    marked: {
      parse: (text) => `<p>${String(text).replace(/\*\*(.*?)\*\*/g, '<strong>$1</strong>')}</p>`
    },
    DOMPurify: { sanitize: (html) => html },
    hljs: { highlightElement: () => {}, getLanguage: () => false }
  };
  global.document = {
    createElement: (tag) => tag === 'template' ? templateElement() : element(),
    addEventListener: () => {}
  };
  global.navigator = {};

  delete require.cache[require.resolve('./markdown-renderer.js')];
  require('./markdown-renderer.js');
  return { renderer: global.window.MarkdownRenderer, element };
}

test('MarkdownRenderer renders markdown as sanitized HTML and keeps raw text for copy', () => {
  const { renderer, element } = loadRenderer();
  const target = element();

  renderer.render(target, '**OpenAI API**\n\n```http\nAuthorization: Bearer KEY\n```');

  assert.equal(target._rawText, '**OpenAI API**\n\n```http\nAuthorization: Bearer KEY\n```');
  assert.match(target.innerHTML, /<strong>OpenAI API<\/strong>/);
});

test('MarkdownRenderer renders reasoning separately from merged response content', () => {
  const { renderer, element } = loadRenderer();
  const target = element();
  target.className = 'upstream-merged-markdown';
  const bubble = element();
  bubble.className = 'chat-message chat-message--assistant';
  const content = element();
  content.className = 'chat-message-content';
  bubble.appendChild(content);
  target.appendChild(bubble);

  renderer.renderResponse(target, {
    reasoning: '检查上游返回',
    content: '**最终回答**'
  });

  const thinking = bubble.querySelector('.chat-thinking');
  assert.ok(thinking);
  assert.equal(thinking.querySelector('.chat-thinking-content').textContent, '检查上游返回');
  assert.match(content.innerHTML, /<strong>最终回答<\/strong>/);
  assert.equal(target._rawText, '检查上游返回\n\n**最终回答**');
});

test('MarkdownRenderer renders tool diagnostics in a collapsed block', () => {
  const { renderer, element } = loadRenderer();
  const target = element();
  target.className = 'upstream-merged-markdown';
  const bubble = element();
  bubble.className = 'chat-message chat-message--assistant';
  const content = element();
  content.className = 'chat-message-content';
  bubble.appendChild(content);
  target.appendChild(bubble);

  renderer.renderResponse(target, {
    content: '**最终回答**',
    tools: '### exec_command\n\n```bash\necho hidden\n```'
  });

  const toolCalls = bubble.querySelector('.chat-tool-calls');
  assert.ok(toolCalls);
  assert.equal(Boolean(toolCalls.open), false);
  assert.match(content.innerHTML, /<strong>最终回答<\/strong>/);
  assert.doesNotMatch(content.innerHTML, /echo hidden/);

  const toolContent = toolCalls.querySelector('.chat-tool-calls-content');
  assert.equal(toolContent._rawText, '### exec_command\n\n```bash\necho hidden\n```');
  assert.match(toolContent.innerHTML, /echo hidden/);
  assert.equal(target._rawText, '**最终回答**\n\n### exec_command\n\n```bash\necho hidden\n```');
});

test('MarkdownRenderer hides empty response content when only tool diagnostics exist', () => {
  const { renderer, element } = loadRenderer();
  const target = element();
  target.className = 'upstream-merged-markdown';
  const bubble = element();
  bubble.className = 'chat-message chat-message--assistant';
  const content = element();
  content.className = 'chat-message-content';
  bubble.appendChild(content);
  target.appendChild(bubble);

  renderer.renderResponse(target, {
    content: '',
    tools: '### exec_command\n\n```bash\necho hidden\n```'
  });

  assert.equal(content.hidden, true);
  assert.equal(content.innerHTML, '');
  assert.equal(content.textContent, '');
  assert.ok(bubble.querySelector('.chat-tool-calls'));
});
