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
      [Symbol.iterator]: () => values[Symbol.iterator](),
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
    if (selector === 'span') return node.tagName === 'SPAN';
    return node.tagName === selector.toUpperCase();
  };

  const textNode = (value = '') => ({
    tagName: '',
    children: [],
    parentNode: null,
    parentElement: null,
    nodeValue: String(value || ''),
    get textContent() { return this.nodeValue; },
    set textContent(next) {
      this.nodeValue = String(next || '');
    }
  });

  const decodeHTML = (value) => String(value || '')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&');

  const stripTags = (value) => String(value || '').replace(/<[^>]+>/g, '');

  const appendInlineHTML = (parent, html) => {
    const pattern = /<span class="([^"]*)">([\s\S]*?)<\/span>/g;
    let offset = 0;
    let match;
    let found = false;
    while ((match = pattern.exec(html)) !== null) {
      found = true;
      if (match.index > offset) parent.appendChild(textNode(decodeHTML(stripTags(html.slice(offset, match.index)))));
      const span = element('span');
      span.className = match[1];
      span.textContent = decodeHTML(stripTags(match[2]));
      parent.appendChild(span);
      offset = match.index + match[0].length;
    }
    if (offset < html.length) parent.appendChild(textNode(decodeHTML(stripTags(html.slice(offset)))));
    return found;
  };

  const element = (tag = 'div') => {
    const el = {
      tagName: tag.toUpperCase(),
      children: [],
      parentNode: null,
      parentElement: null,
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
      set innerHTML(value) {
        this._html = String(value || '');
        this.children = [];
        const codeMatch = this._html.match(/<pre><code class="([^"]*)">([\s\S]*?)<\/code><\/pre>/);
        if (codeMatch) {
          const pre = element('pre');
          const code = element('code');
          code.className = codeMatch[1];
          code.textContent = codeMatch[2]
            .replace(/&lt;/g, '<')
            .replace(/&gt;/g, '>')
            .replace(/&amp;/g, '&');
          pre.appendChild(code);
          this.appendChild(pre);
          return;
        }
        if (appendInlineHTML(this, this._html)) return;
        this._text = decodeHTML(stripTags(this._html));
      },
      get innerHTML() { return this._html; },
      set textContent(value) {
        this.children.forEach(child => {
          child.parentNode = null;
          child.parentElement = null;
        });
        this.children = [];
        this._text = String(value || '');
      },
      get textContent() {
        if (this.children.length > 0) return this.children.map(child => child.textContent || '').join('');
        return this._text;
      },
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
        child.parentElement = this;
        this.children.push(child);
        return child;
      },
      insertBefore(child, ref) {
        child.parentNode = this;
        child.parentElement = this;
        const index = this.children.indexOf(ref);
        if (index === -1) this.children.push(child);
        else this.children.splice(index, 0, child);
        return child;
      },
      replaceChildren(...children) {
        this.children.forEach(child => {
          child.parentNode = null;
          child.parentElement = null;
        });
        this.children = [];
        children.forEach(child => this.appendChild(child));
      },
      querySelector(selector) {
        return this.querySelectorAll(selector)[0] || null;
      },
      querySelectorAll(selector) {
        const found = [];
        const visit = (node) => {
          (node.children || []).forEach(child => {
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
    createElement: (tag) => tag === 'template' ? templateElement() : element(tag),
    createTextNode: textNode,
    createDocumentFragment: () => element('fragment'),
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

test('MarkdownRenderer adds nested syntax tokens inside apply_patch diff blocks', () => {
  const { renderer, element } = loadRenderer();
  const target = element();
  global.window.marked.parse = (text) => {
    const code = String(text).match(/```diff\n([\s\S]*?)\n```/)?.[1] || '';
    return `<pre><code class="language-diff">${code}</code></pre>`;
  };

  renderer.render(target, [
    '```diff',
    '*** Begin Patch',
    '*** Add File: demo.go',
    '+package main',
    '+const name = "ccLoad"',
    '*** End Patch',
    '```'
  ].join('\n'));

  const code = target.querySelector('code');
  assert.equal(code.className, 'language-diff');
  assert.match(code.textContent, /package main/);
  const classes = code.querySelectorAll('span').map(span => span.className).join(' ');
  assert.match(classes, /chat-patch-meta/);
  assert.match(classes, /chat-patch-keyword/);
  assert.match(classes, /chat-patch-string/);
});

test('MarkdownRenderer highlights apply_patch code with the patch file language', () => {
  const { renderer, element } = loadRenderer();
  const target = element();
  global.window.marked.parse = (text) => {
    const code = String(text).match(/```diff\n([\s\S]*?)\n```/)?.[1] || '';
    return `<pre><code class="language-diff">${code}</code></pre>`;
  };
  global.window.hljs = {
    highlightElement: () => {},
    getLanguage: (language) => language === 'diff' || language === 'go',
    highlight: (source, options) => {
      assert.equal(options.language, 'go');
      return {
        value: String(source)
          .replace(/\bpackage\b/g, '<span class="hljs-keyword">package</span>')
          .replace(/"ccLoad"/g, '<span class="hljs-string">"ccLoad"</span>')
      };
    }
  };

  renderer.render(target, [
    '```diff',
    '*** Begin Patch',
    '*** Add File: demo.go',
    '+package main',
    '+const name = "ccLoad"',
    '*** End Patch',
    '```'
  ].join('\n'));

  const code = target.querySelector('code');
  assert.match(code.textContent, /package main/);
  const classes = code.querySelectorAll('span').map(span => span.className).join(' ');
  assert.match(classes, /chat-patch-add-marker/);
  assert.match(classes, /hljs-keyword/);
  assert.match(classes, /hljs-string/);
});
