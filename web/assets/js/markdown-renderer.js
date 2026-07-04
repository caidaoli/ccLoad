/**
 * Shared Markdown renderer for assistant responses.
 * Uses the same Markdown, sanitization, highlighting, and code-block chrome
 * for chat bubbles and merged upstream/debug responses.
 */
(function () {
  'use strict';

  const MARKDOWN_ALLOWED_TAGS = [
    'a', 'blockquote', 'br', 'code', 'del', 'em', 'hr', 'li', 'ol', 'p',
    'pre', 'strong', 'table', 'tbody', 'td', 'th', 'thead', 'tr', 'ul'
  ];
  const MARKDOWN_ALLOWED_ATTR = ['align', 'class', 'href', 'title'];

  const CODE_LANGUAGE_META = {
    bash: { icon: 'SH', name: 'Bash' },
    c: { icon: 'C', name: 'C' },
    cpp: { icon: 'C++', name: 'Cpp' },
    cxx: { icon: 'C++', name: 'Cpp' },
    'c++': { icon: 'C++', name: 'Cpp' },
    css: { icon: 'CSS', name: 'CSS' },
    go: { icon: 'Go', name: 'Go' },
    html: { icon: 'HTML', name: 'HTML' },
    http: { icon: 'HTTP', name: 'HTTP' },
    java: { icon: 'Java', name: 'Java' },
    js: { icon: 'JS', name: 'JavaScript' },
    javascript: { icon: 'JS', name: 'JavaScript' },
    json: { icon: '{}', name: 'JSON' },
    markdown: { icon: 'MD', name: 'Markdown' },
    md: { icon: 'MD', name: 'Markdown' },
    php: { icon: 'PHP', name: 'PHP' },
    py: { icon: 'Py', name: 'Python' },
    python: { icon: 'Py', name: 'Python' },
    rust: { icon: 'Rs', name: 'Rust' },
    rs: { icon: 'Rs', name: 'Rust' },
    shell: { icon: 'SH', name: 'Shell' },
    sh: { icon: 'SH', name: 'Shell' },
    sql: { icon: 'SQL', name: 'SQL' },
    ts: { icon: 'TS', name: 'TypeScript' },
    typescript: { icon: 'TS', name: 'TypeScript' },
    xml: { icon: 'XML', name: 'XML' },
    yaml: { icon: 'YAML', name: 'YAML' },
    yml: { icon: 'YAML', name: 'YAML' }
  };

  const CODE_COPY_ICON_HTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>';

  function label(key, fallback) {
    if (typeof window.i18nText === 'function') return window.i18nText(key, fallback);
    if (typeof window.t === 'function') return window.t(key) || fallback;
    return fallback;
  }

  function sanitizeMarkdownHTML(html) {
    if (typeof window.DOMPurify === 'undefined' || typeof window.DOMPurify.sanitize !== 'function') {
      return null;
    }

    const clean = window.DOMPurify.sanitize(String(html || ''), {
      ALLOWED_TAGS: MARKDOWN_ALLOWED_TAGS,
      ALLOWED_ATTR: MARKDOWN_ALLOWED_ATTR,
      ALLOW_ARIA_ATTR: false,
      ALLOW_DATA_ATTR: false
    });

    return normalizeMarkdownHTML(clean);
  }

  function normalizeMarkdownHTML(html) {
    const template = document.createElement('template');
    template.innerHTML = String(html || '');

    template.content.querySelectorAll('a').forEach((anchor) => {
      if (!isSafeMarkdownHref(anchor.getAttribute('href'))) {
        anchor.removeAttribute('href');
        return;
      }
      anchor.setAttribute('target', '_blank');
      anchor.setAttribute('rel', 'noopener noreferrer');
    });

    template.content.querySelectorAll('[href]').forEach((element) => {
      if (element.tagName.toUpperCase() !== 'A') element.removeAttribute('href');
    });

    template.content.querySelectorAll('[title]').forEach((element) => {
      if (element.tagName.toUpperCase() !== 'A') element.removeAttribute('title');
    });

    template.content.querySelectorAll('[class]').forEach((element) => {
      const className = element.getAttribute('class') || '';
      const isLanguageClass = /^language-[a-z0-9_-]+$/i.test(className);
      if (element.tagName.toUpperCase() !== 'CODE' || !isLanguageClass) {
        element.removeAttribute('class');
      }
    });

    template.content.querySelectorAll('[align]').forEach((element) => {
      const tagName = element.tagName.toUpperCase();
      const isTableCell = tagName === 'TD' || tagName === 'TH';
      const isAllowedAlign = /^(left|center|right)$/i.test(element.getAttribute('align') || '');
      if (!isTableCell || !isAllowedAlign) element.removeAttribute('align');
    });

    return template.innerHTML;
  }

  function isSafeMarkdownHref(href) {
    const value = String(href || '').trim();
    if (!value) return false;
    if (value.startsWith('#') || value.startsWith('/')) return true;
    try {
      const parsed = new URL(value, window.location.origin);
      return parsed.protocol === 'http:' || parsed.protocol === 'https:' || parsed.protocol === 'mailto:';
    } catch (_) {
      return false;
    }
  }

  function getRenderTarget(target) {
    const el = typeof target === 'string' ? document.getElementById(target) : target;
    if (!el) return null;
    if (!el.classList?.contains('upstream-merged-markdown')) return el;

    return getMergedResponseContent(el);
  }

  function createAssistantBubble() {
    const bubble = document.createElement('div');
    bubble.className = 'chat-message chat-message--assistant';
    return bubble;
  }

  function createMessageContent() {
    const content = document.createElement('div');
    content.className = 'chat-message-content';
    return content;
  }

  function getMergedResponseBubble(el) {
    let bubble = el.querySelector?.('.chat-message');
    if (bubble) return bubble;

    bubble = createAssistantBubble();
    const existingContent = el.querySelector?.('.chat-message-content');
    if (existingContent) {
      el.replaceChildren(bubble);
      bubble.appendChild(existingContent);
      return bubble;
    }

    bubble.appendChild(createMessageContent());
    el.replaceChildren(bubble);
    return bubble;
  }

  function getMergedResponseContent(el) {
    const bubble = getMergedResponseBubble(el);
    let content = bubble.querySelector?.('.chat-message-content');
    if (!content) {
      content = createMessageContent();
      bubble.appendChild(content);
    }
    return content;
  }

  function normalizeResponseParts(response) {
    if (response && typeof response === 'object' && !Array.isArray(response)) {
      return {
        reasoning: String(response.reasoning || response.thinking || ''),
        content: String(response.content ?? response.text ?? '')
      };
    }
    return { reasoning: '', content: String(response || '') };
  }

  function mergedRawText(parts) {
    return [parts.reasoning, parts.content].filter(Boolean).join('\n\n');
  }

  function renderThinking(bubble, thinking, streaming = false) {
    if (!bubble) return;
    const text = String(thinking || '').trim();
    let thinkingEl = bubble.querySelector?.('.chat-thinking');
    if (!text) {
      thinkingEl?.remove?.();
      return;
    }

    if (!thinkingEl) {
      thinkingEl = document.createElement('details');
      thinkingEl.className = 'chat-thinking';

      const summary = document.createElement('summary');
      summary.className = 'chat-thinking-summary';
      summary.setAttribute('data-i18n', 'modelTest.chat.thinking');
      summary.textContent = label('modelTest.chat.thinking', '思考');

      const contentEl = document.createElement('div');
      contentEl.className = 'chat-thinking-content';

      thinkingEl.appendChild(summary);
      thinkingEl.appendChild(contentEl);
      bubble.insertBefore(thinkingEl, bubble.firstChild || null);
    }

    const contentEl = thinkingEl.querySelector?.('.chat-thinking-content');
    if (contentEl) contentEl.textContent = text;
    thinkingEl.open = !!streaming;
  }

  function renderResponse(target, response, options = {}) {
    const el = typeof target === 'string' ? document.getElementById(target) : target;
    if (!el) return;

    const parts = normalizeResponseParts(response);
    const rawText = mergedRawText(parts);
    el._rawText = rawText;

    const bubble = el.classList?.contains('upstream-merged-markdown')
      ? getMergedResponseBubble(el)
      : el.closest?.('.chat-message');
    const contentTarget = el.classList?.contains('upstream-merged-markdown')
      ? getMergedResponseContent(el)
      : getRenderTarget(el);

    renderThinking(bubble, parts.reasoning, options.streaming === true);
    render(contentTarget, parts.content, options);
    el._rawText = rawText;
    if (bubble) bubble._rawText = rawText;
  }

  function render(target, markdown, options = {}) {
    const el = typeof target === 'string' ? document.getElementById(target) : target;
    if (!el) return;

    const text = String(markdown || '');
    el._rawText = text;
    const renderTarget = getRenderTarget(el);
    if (!renderTarget) return;

    renderTarget.closest?.('.chat-message')?.classList.remove('chat-message--has-code');
    if (typeof window.marked !== 'undefined' && typeof window.marked.parse === 'function') {
      const sanitizedHTML = sanitizeMarkdownHTML(window.marked.parse(text));
      if (sanitizedHTML === null) {
        renderTarget.textContent = text;
      } else {
        renderTarget.innerHTML = sanitizedHTML;
      }

      if (typeof window.hljs !== 'undefined') {
        renderTarget.querySelectorAll('pre code').forEach((block) => {
          if (shouldHighlightCodeWithHljs(block)) window.hljs.highlightElement(block);
          enhanceCodeSyntax(block);
        });
      }
      enhanceCodeBlocks(renderTarget);
    } else {
      renderTarget.textContent = text;
    }

    if (options.cursor) {
      const cursor = document.createElement('span');
      cursor.className = 'chat-cursor';
      renderTarget.appendChild(cursor);
    }
  }

  function enhanceCodeBlocks(target) {
    if (!target || typeof target.querySelectorAll !== 'function') return;

    let hasCodeBlock = false;
    target.querySelectorAll('pre > code').forEach((codeEl) => {
      const preEl = codeEl.parentElement;
      if (!preEl || preEl.closest('.chat-code-block')) return;

      hasCodeBlock = true;
      const blockEl = document.createElement('div');
      blockEl.className = 'chat-code-block';

      const headerEl = document.createElement('div');
      headerEl.className = 'chat-code-header';

      const languageMeta = getCodeLanguageMeta(codeEl);
      const languageEl = document.createElement('span');
      languageEl.className = 'chat-code-language';
      languageEl.setAttribute('aria-label', languageMeta.name);
      const languageIconEl = document.createElement('span');
      languageIconEl.className = 'chat-code-language-icon';
      languageIconEl.setAttribute('aria-hidden', 'true');
      languageIconEl.textContent = languageMeta.icon;
      const languageNameEl = document.createElement('span');
      languageNameEl.className = 'chat-code-language-name';
      languageNameEl.textContent = languageMeta.name;
      languageEl.appendChild(languageIconEl);
      languageEl.appendChild(languageNameEl);
      headerEl.appendChild(languageEl);

      const actionsEl = document.createElement('div');
      actionsEl.className = 'chat-code-actions';
      actionsEl.appendChild(createCodeCopyButton(codeEl));
      headerEl.appendChild(actionsEl);

      const bodyEl = document.createElement('div');
      bodyEl.className = 'chat-code-body';

      const lineNumbersEl = buildCodeLineNumbers(codeEl);
      preEl.classList.add('chat-code-pre');

      preEl.parentNode.insertBefore(blockEl, preEl);
      bodyEl.appendChild(lineNumbersEl);
      bodyEl.appendChild(preEl);
      blockEl.appendChild(headerEl);
      blockEl.appendChild(bodyEl);
    });

    const bubbleEl = target.closest?.('.chat-message');
    if (bubbleEl && hasCodeBlock) bubbleEl.classList.add('chat-message--has-code');
  }

  function getCodeLanguageMeta(codeEl) {
    const rawLanguage = getCodeLanguage(codeEl);
    if (!rawLanguage) return { icon: '{}', name: 'Code' };

    const normalized = rawLanguage.trim().toLowerCase();
    if (normalized === 'http' && isHeaderOnlyCode(codeEl?.textContent || '')) {
      return { icon: 'HDR', name: 'Headers' };
    }
    if (CODE_LANGUAGE_META[normalized]) return CODE_LANGUAGE_META[normalized];

    const name = rawLanguage
      .replace(/[-_]+/g, ' ')
      .replace(/\b[a-z]/g, (letter) => letter.toUpperCase());
    return { icon: rawLanguage.slice(0, 4), name };
  }

  function isHeaderOnlyCode(text) {
    const lines = String(text || '').split('\n').map((line) => line.trim()).filter(Boolean);
    if (lines.length === 0) return false;
    return lines.every((line) => /^[A-Za-z0-9-]+:\s*\S/.test(line));
  }

  function getCodeLanguage(codeEl) {
    if (!codeEl || !codeEl.classList) return '';
    for (const className of codeEl.classList) {
      const match = className.match(/^language-(.+)$/i);
      if (match && match[1]) return match[1];
    }
    return '';
  }

  function createCodeCopyButton(codeEl) {
    const btn = document.createElement('button');
    const copyLabel = label('common.copy', '复制');
    btn.type = 'button';
    btn.className = 'chat-code-copy-btn';
    btn.setAttribute('aria-label', copyLabel);
    btn.title = copyLabel;
    btn.innerHTML = CODE_COPY_ICON_HTML;
    btn._chatCodeText = getCodePlainText(codeEl);
    return btn;
  }

  function getCodePlainText(codeEl) {
    return String(codeEl?.textContent || '').replace(/\n$/, '');
  }

  function shouldHighlightCodeWithHljs(codeEl) {
    const language = getCodeLanguage(codeEl).trim().toLowerCase();
    if (!language) return true;
    if (!window.hljs || typeof window.hljs.getLanguage !== 'function') return true;
    return Boolean(window.hljs.getLanguage(language));
  }

  function enhanceCodeSyntax(codeEl) {
    const language = getCodeLanguage(codeEl).trim().toLowerCase();
    const text = codeEl?.textContent || '';
    if (language === 'http' && isHeaderOnlyCode(text)) {
      renderHeaderCode(codeEl);
      return;
    }
    if (language === 'bash' || language === 'sh' || language === 'shell') enhanceShellCode(codeEl);
  }

  function renderHeaderCode(codeEl) {
    const source = getCodePlainText(codeEl);
    codeEl.textContent = '';
    source.split('\n').forEach((line, index) => {
      if (index > 0) codeEl.appendChild(document.createTextNode('\n'));
      const match = line.match(/^(\s*)([A-Za-z0-9-]+)(\s*:\s*)(.*)$/);
      if (!match) {
        codeEl.appendChild(document.createTextNode(line));
        return;
      }
      const [, indent, key, separator, value] = match;
      codeEl.appendChild(document.createTextNode(indent));
      codeEl.appendChild(createCodeToken('chat-header-key', key));
      codeEl.appendChild(document.createTextNode(separator));
      codeEl.appendChild(createCodeToken('chat-header-value', value));
    });
  }

  function enhanceShellCode(codeEl) {
    const walker = document.createTreeWalker(codeEl, NodeFilter.SHOW_TEXT, {
      acceptNode(node) {
        if (node.parentElement?.closest?.('.hljs-string')) return NodeFilter.FILTER_REJECT;
        return /(\bcurl\b|https?:\/\/|--?[A-Za-z][\w-]*)/.test(node.nodeValue || '')
          ? NodeFilter.FILTER_ACCEPT
          : NodeFilter.FILTER_REJECT;
      }
    });
    const nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    nodes.forEach((node) => {
      node.parentNode.replaceChild(buildShellTokenFragment(node.nodeValue || ''), node);
    });
  }

  function buildShellTokenFragment(text) {
    const fragment = document.createDocumentFragment();
    const pattern = /(https?:\/\/[^\s\\'"]+|\bcurl\b|--?[A-Za-z][\w-]*)/g;
    let offset = 0;
    let match;
    while ((match = pattern.exec(text)) !== null) {
      if (match.index > offset) fragment.appendChild(document.createTextNode(text.slice(offset, match.index)));
      const token = match[0];
      const className = token === 'curl'
        ? 'chat-shell-command'
        : token.startsWith('http')
          ? 'chat-shell-url'
          : 'chat-shell-flag';
      fragment.appendChild(createCodeToken(className, token));
      offset = match.index + token.length;
    }
    if (offset < text.length) fragment.appendChild(document.createTextNode(text.slice(offset)));
    return fragment;
  }

  function createCodeToken(className, text) {
    const tokenEl = document.createElement('span');
    tokenEl.className = className;
    tokenEl.textContent = text;
    return tokenEl;
  }

  function buildCodeLineNumbers(codeEl) {
    const gutterEl = document.createElement('div');
    gutterEl.className = 'chat-code-line-numbers';
    gutterEl.setAttribute('aria-hidden', 'true');

    const source = String(codeEl?.textContent || '').replace(/\n$/, '');
    const lineCount = Math.max(1, source.split('\n').length);
    for (let lineNumber = 1; lineNumber <= lineCount; lineNumber++) {
      const lineEl = document.createElement('span');
      lineEl.textContent = String(lineNumber);
      gutterEl.appendChild(lineEl);
    }

    return gutterEl;
  }

  function copyText(text) {
    if (window.copyToClipboard) return window.copyToClipboard(text);
    const clipboard = typeof navigator !== 'undefined' && navigator.clipboard;
    if (clipboard && typeof clipboard.writeText === 'function') return clipboard.writeText(text);
    return Promise.reject(new Error('copy failed'));
  }

  function markCopied(btn) {
    const copiedLabel = label('channels.batchRefreshCopied', '已复制');
    const originalTitle = btn.title;
    const originalLabel = btn.getAttribute('aria-label');
    btn.classList.add('chat-code-copy-btn--copied');
    btn.title = copiedLabel;
    btn.setAttribute('aria-label', copiedLabel);
    setTimeout(() => {
      btn.classList.remove('chat-code-copy-btn--copied');
      btn.title = originalTitle;
      if (originalLabel) btn.setAttribute('aria-label', originalLabel);
    }, 1500);
  }

  function bindCopyHandler() {
    if (window.__markdownRendererCopyBound || typeof document === 'undefined') return;
    window.__markdownRendererCopyBound = true;
    document.addEventListener('click', (event) => {
      const btn = event.target.closest?.('.chat-code-copy-btn');
      if (!btn) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      const text = btn._chatCodeText || btn.closest('.chat-code-block')?.querySelector('pre code')?.textContent || '';
      if (!text) return;
      copyText(text).then(() => markCopied(btn)).catch(() => {});
    });
  }

  window.MarkdownRenderer = { render, renderResponse };
  bindCopyHandler();
})();
