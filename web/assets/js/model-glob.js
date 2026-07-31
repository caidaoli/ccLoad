/**
 * 模型名通配匹配，与后端 internal/model/config.go 的 matchModelGlob / IsModelPattern 保持一致。
 * 仅识别 '*'（任意串，含空）与 '?'（单个字符）两个通配元字符，其余字符精确匹配；
 * 不支持字符类 [...] 以避免模型名中字面方括号/问号的误匹配。
 *
 * 按 Unicode code point 匹配（Array.from 按 code point 拆分，等价 Go 的 []rune），
 * 因此 '?' 匹配一个字符（code point）而非一个 UTF-16 code unit；大小写敏感。
 * 旧实现按 string 索引（UTF-16 code unit）迭代，会对代理对字符（如 😀 = U+1F600，
 * 在 JS 中占 2 个 code unit）产生错位，导致 model-? 对 model-😀 后端匹配而前端不匹配。
 */
(function (globalThisRef) {
  'use strict';

  /** 是否为通配模式（含 '*' 或 '?'），与后端 IsModelPattern 一致。 */
  function isModelPattern(s) {
    return typeof s === 'string' && (s.indexOf('*') !== -1 || s.indexOf('?') !== -1);
  }

  /**
   * 通配匹配：仅识别 '*'（任意串，含空）与 '?'（单字符），按 code point 回溯匹配。
   * @param {string} pattern 模型模式（可能含通配符）
   * @param {string} name 待匹配的具体模型名
   * @returns {boolean}
   */
  function matchModelGlob(pattern, name) {
    if (typeof pattern !== 'string' || typeof name !== 'string') return false;
    // Array.from 按 Unicode code point 拆分，与 Go []rune 语义一致
    const pr = Array.from(pattern);
    const nr = Array.from(name);
    let p = 0, n = 0, starP = -1, starN = 0;
    while (n < nr.length) {
      if (p < pr.length && pr[p] === '*') { starP = p; starN = n; p++; }
      else if (p < pr.length && (pr[p] === nr[n] || pr[p] === '?')) { p++; n++; }
      else if (starP >= 0) { p = starP + 1; starN++; n = starN; } // 回溯：'*' 多吃一个字符后重试
      else return false;
    }
    while (p < pr.length && pr[p] === '*') p++; // 尾部连续 '*' 视为匹配空
    return p === pr.length; // 剩余若有 '?' 则不匹配（'?' 必须吃一个字符）
  }

  const api = { matchModelGlob, isModelPattern };

  // 浏览器：挂到 window；Node 测试：挂到 global，便于被测源文件裸引用
  const root = typeof globalThisRef !== 'undefined' ? globalThisRef
    : (typeof globalThis !== 'undefined' ? globalThis : undefined);
  if (root) {
    root.matchModelGlob = matchModelGlob;
    root.isModelPattern = isModelPattern;
  }
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof globalThis !== 'undefined' ? globalThis : undefined);
