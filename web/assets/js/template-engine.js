/**
 * 轻量级模板引擎
 * 使用原生 HTML <template> 元素实现 HTML/JS 分离
 *
 * 用法:
 *   1. 在 HTML 中定义 <template id="tpl-xxx">...</template>
 *   2. 模板内使用 {{key}} 或 {{obj.key}} 语法绑定数据
 *   3. JS 中调用 TemplateEngine.render('tpl-xxx', data)
 *
 * 特性:
 *   - 自动 HTML 转义防止 XSS
 *   - 支持嵌套属性访问 (obj.nested.value)
 *   - 支持 {{{raw}}} 语法插入原始 HTML (慎用)
 *   - 模板缓存提升性能
 */
const TemplateEngine = {
  // 模板缓存
  _cache: new Map(),

  /**
   * 获取模板内容 (带缓存)
   * @param {string} id - 模板ID (含或不含#前缀均可)
   * @returns {string} 模板HTML字符串
   */
  _getTemplate(id) {
    const templateId = id.startsWith('#') ? id.slice(1) : id;

    if (!this._cache.has(templateId)) {
      const tpl = document.getElementById(templateId);
      if (!tpl) {
        console.error(`[TemplateEngine] Template not found: ${templateId}`);
        return '';
      }
      // 缓存模板HTML字符串
      this._cache.set(templateId, tpl.innerHTML.trim());
    }
    return this._cache.get(templateId);
  },

  /**
   * HTML转义 (防XSS)
   * @param {string} str - 原始字符串
   * @returns {string} 转义后的字符串
   */
  _escape(str) {
    if (str === null || str === undefined) return '';
    return String(str).replace(/[&<>"']/g, c => ({
      '&': '&amp;',
      '<': '&lt;',
      '>': '&gt;',
      '"': '&quot;',
      "'": '&#39;'
    }[c]));
  },

  /**
   * 从对象中获取嵌套属性值
   * @param {Object} obj - 数据对象
   * @param {string} path - 属性路径 (如 "user.name")
   * @returns {*} 属性值
   */
  _getValue(obj, path) {
    return path.split('.').reduce((o, k) => o?.[k], obj);
  },

  /**
   * 渲染单个模板
   * @param {string} id - 模板ID
   * @param {Object} data - 数据对象
   * @returns {HTMLElement|null} 渲染后的DOM元素
   */
  render(id, data) {
    let html = this._getTemplate(id);
    if (!html) return null;

    // 处理 {{{raw}}} 语法 (原始HTML，不转义)
    html = html.replace(/\{\{\{(\w+(?:\.\w+)*)\}\}\}/g, (_, path) => {
      const value = this._getValue(data, path);
      return value !== undefined ? String(value) : '';
    });

    // 处理 {{key}} 语法 (自动转义)
    html = html.replace(/\{\{(\w+(?:\.\w+)*)\}\}/g, (_, path) => {
      const value = this._getValue(data, path);
      return value !== undefined ? this._escape(value) : '';
    });

    // 创建DOM元素 - 表格元素需要正确的父容器才能被浏览器正确解析
    const trimmed = html.trim().toLowerCase();
    let temp;
    if (trimmed.startsWith('<tr')) {
      temp = document.createElement('tbody');
    } else if (trimmed.startsWith('<td') || trimmed.startsWith('<th')) {
      temp = document.createElement('tr');
    } else if (trimmed.startsWith('<thead') || trimmed.startsWith('<tbody') || trimmed.startsWith('<tfoot')) {
      temp = document.createElement('table');
    } else {
      temp = document.createElement('div');
    }
    temp.innerHTML = html;
    return temp.firstElementChild;
  },

  /**
   * 批量渲染列表
   * @param {string} id - 模板ID
   * @param {Array} items - 数据数组
   * @returns {DocumentFragment} 包含所有渲染元素的片段
   */
  renderList(id, items) {
    const fragment = document.createDocumentFragment();
    if (!Array.isArray(items)) return fragment;

    items.forEach(item => {
      const el = this.render(id, item);
      if (el) fragment.appendChild(el);
    });
    return fragment;
  },

  /**
   * 渲染并替换容器内容
   * @param {string} containerId - 容器元素ID
   * @param {string} templateId - 模板ID
   * @param {Array|Object} data - 数据(数组则批量渲染)
   */
  renderTo(containerId, templateId, data) {
    const container = document.getElementById(containerId);
    if (!container) {
      console.error(`[TemplateEngine] Container not found: ${containerId}`);
      return;
    }

    container.innerHTML = '';
    if (Array.isArray(data)) {
      container.appendChild(this.renderList(templateId, data));
    } else {
      const el = this.render(templateId, data);
      if (el) container.appendChild(el);
    }
  },

  /**
   * 清除模板缓存 (通常在动态添加模板后调用)
   * @param {string} [id] - 指定清除的模板ID，不传则清除所有
   */
  clearCache(id) {
    if (id) {
      this._cache.delete(id.startsWith('#') ? id.slice(1) : id);
    } else {
      this._cache.clear();
    }
  }
};

// 导出为全局变量 (兼容非模块化环境)
window.TemplateEngine = TemplateEngine;
