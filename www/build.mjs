#!/usr/bin/env node
/**
 * www 静态站构建：把源 HTML + 语言包预渲染成两套静态页，输出到 dist/。
 *   /        英文（源 HTML 本身就是英文原文）
 *   /zh/     简体中文（data-i18n* 的键全部由 zh-CN 语言包替换，缺键直接失败）
 * 导航、页脚、canonical / hreflang / Open Graph / JSON-LD、sitemap.xml、robots.txt 均在构建期生成，
 * 搜索引擎拿到的就是最终 HTML，不依赖运行时 JS。零依赖，仅用 Node 内置模块。
 *
 * `node www/build.mjs --indexnow` 把全部页面 URL 提交给 IndexNow（Bing、Yandex 等），发布后由 make www-release 调用。
 */
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const SRC_DIR = path.dirname(fileURLToPath(import.meta.url));
export const SITE_URL = 'https://ccload.xyz';
const GITHUB_URL = 'https://github.com/caidaoli/ccLoad';
// IndexNow 密钥是公开的站点归属证明，构建时写成 /<key>.txt
export const INDEXNOW_KEY = 'b60e5cb0a81bf0fdc1966f224ffa73b7';

export const PAGES = ['index', 'install', 'config', 'usage', 'feedback'];
export const LOCALES = [
  { code: 'en', dir: '', ogLocale: 'en_US', label: 'EN' },
  { code: 'zh-CN', dir: 'zh/', ogLocale: 'zh_CN', label: '中文' }
];

const NAV_ITEMS = [
  { page: 'index', key: 'www.nav.home', icon: 'home' },
  { page: 'install', key: 'www.nav.install', icon: 'download' },
  { page: 'config', key: 'www.nav.config', icon: 'settings' },
  { page: 'usage', key: 'www.nav.usage', icon: 'terminal' },
  { page: 'feedback', key: 'www.nav.feedback', icon: 'message' }
];

const ICONS = {
  home: '<path d="M3 11.5 12 4l9 7.5"/><path d="M5 10.5V20h14v-9.5"/><path d="M9 20v-6h6v6"/>',
  download: '<path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/>',
  settings: '<path d="M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Z"/><path d="M19.4 15a1.8 1.8 0 0 0 .36 1.98l.05.05a2.1 2.1 0 0 1-2.97 2.97l-.05-.05a1.8 1.8 0 0 0-1.98-.36 1.8 1.8 0 0 0-1.08 1.65V21a2.1 2.1 0 0 1-4.2 0v-.07a1.8 1.8 0 0 0-1.08-1.65 1.8 1.8 0 0 0-1.98.36l-.05.05a2.1 2.1 0 0 1-2.97-2.97l.05-.05A1.8 1.8 0 0 0 4.6 15a1.8 1.8 0 0 0-1.65-1.08H3a2.1 2.1 0 0 1 0-4.2h.07A1.8 1.8 0 0 0 4.72 8.65a1.8 1.8 0 0 0-.36-1.98l-.05-.05a2.1 2.1 0 0 1 2.97-2.97l.05.05a1.8 1.8 0 0 0 1.98.36A1.8 1.8 0 0 0 10.4 2.4V2.1a2.1 2.1 0 0 1 4.2 0v.3a1.8 1.8 0 0 0 1.08 1.65 1.8 1.8 0 0 0 1.98-.36l.05-.05a2.1 2.1 0 0 1 2.97 2.97l-.05.05a1.8 1.8 0 0 0-.36 1.98 1.8 1.8 0 0 0 1.65 1.08H22a2.1 2.1 0 0 1 0 4.2h-.07A1.8 1.8 0 0 0 19.4 15Z"/>',
  terminal: '<path d="m4 17 6-6-6-6"/><path d="M12 19h8"/>',
  message: '<path d="M21 12a8 8 0 0 1-8 8H6l-3 3v-7a8 8 0 1 1 18-4Z"/>'
};

const GITHUB_ICON = '<path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/>';
const LANG_ICON = '<path d="M12.87 15.07 10.33 12.56l.03-.03A17.52 17.52 0 0 0 14.07 6H17V4h-7V2H8v2H1v2h11.17C11.5 7.92 10.44 9.75 9 11.35 8.07 10.32 7.3 9.19 6.69 8h-2c.73 1.63 1.73 3.17 2.98 4.56L2.58 17.58 4 19l5-5 3.11 3.11.76-2.04ZM18.5 10h-2L12 22h2l1.12-3h4.75L21 22h2l-4.5-12Zm-2.62 7 1.62-4.33L19.12 17h-3.24Z"/>';

// 发布到站点的静态资源；语言包只在构建期使用，不发布
const STATIC_FILES = ['favicon.svg', 'favicon.ico', 'apple-touch-icon.png', 'brand-mark.svg', 'brand-wordmark.svg'];
const STATIC_DIRS = ['assets/css', 'assets/js', 'assets/images', 'assets/video'];
// 只对这些相对资源路径加 ../ 前缀；页面间链接（*.html、./）在 /zh/ 下保持同级跳转
const ASSET_URL = /^(assets\/|[\w-]+\.(?:svg|ico|png)(?:[?#]|$))/;
const ATTR_KEYS = { content: 'content', alt: 'alt', 'aria-label': 'aria-label', title: 'title', src: 'src', poster: 'poster' };

function escapeHtml(value) {
  return String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function decodeText(html) {
  return html.replace(/<[^>]+>/g, '').replace(/\s+/g, ' ').trim()
    .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');
}

export function loadMessages(localesDir = path.join(SRC_DIR, 'assets/locales')) {
  const window = {};
  const context = vm.createContext({ window });
  for (const file of fs.readdirSync(localesDir).filter(f => f.endsWith('.js')).sort()) {
    const filename = path.join(localesDir, file);
    vm.runInContext(fs.readFileSync(filename, 'utf8'), context, { filename });
  }
  return window.I18N_LOCALES || {};
}

function pageHref(page) {
  return page === 'index' ? './' : `${page}.html`;
}

function localeHref(from, to, page) {
  const target = `${to.dir}${page === 'index' ? '' : `${page}.html`}`;
  return from.dir ? `../${target}` : (target || './');
}

function pageUrl(page, locale) {
  return `${SITE_URL}/${locale.dir}${page === 'index' ? '' : `${page}.html`}`;
}

// 找到与 from 之前那个开标签配对的闭标签位置（同名标签按深度计数）
function findClose(html, tag, from) {
  const re = new RegExp(`<(/?)${tag}\\b[^>]*>`, 'gi');
  re.lastIndex = from;
  let depth = 1;
  let match;
  while ((match = re.exec(html))) {
    depth += match[1] ? -1 : 1;
    if (depth === 0) return match.index;
  }
  throw new Error(`unclosed <${tag}> at offset ${from}`);
}

function rewriteInner(html, attr, render) {
  const open = new RegExp(`<([a-zA-Z][\\w-]*)\\b[^>]*\\s${attr}="([^"]+)"[^>]*>`, 'g');
  let out = '';
  let last = 0;
  let match;
  while ((match = open.exec(html))) {
    const start = match.index + match[0].length;
    const end = findClose(html, match[1], start);
    out += html.slice(last, start) + render(match[2], html.slice(start, end));
    last = end;
    open.lastIndex = end;
  }
  return out + html.slice(last);
}

function setAttr(tag, name, value) {
  const re = new RegExp(`(\\s${name}=")[^"]*(")`);
  if (re.test(tag)) return tag.replace(re, `$1${value}$2`);
  return tag.replace(/\s*(\/?)>$/, ` ${name}="${value}"$1>`);
}

/**
 * 按语言渲染 data-i18n*：
 * - en：源 HTML 即原文，只有 lookup 命中时才替换
 * - 其他语言：每个键都必须有译文，缺失收集后统一报错
 */
export function translate(html, lookup, { strict }) {
  const missing = new Set();
  const resolve = (key, fallback) => {
    const value = lookup(key);
    if (value !== undefined) return value;
    if (strict) missing.add(key);
    return fallback;
  };

  let out = rewriteInner(html, 'data-i18n-html', (key, inner) => resolve(key, inner));
  out = rewriteInner(out, 'data-i18n', (key, inner) => {
    const value = resolve(key, null);
    return value === null ? inner : escapeHtml(value);
  });
  out = out.replace(/<[a-zA-Z][^>]*\sdata-i18n-[\w-]+="[^"]*"[^>]*>/g, tag => {
    let result = tag;
    for (const [, kind, key] of tag.matchAll(/\sdata-i18n-([\w-]+)="([^"]*)"/g)) {
      if (kind === 'html') continue;
      const target = ATTR_KEYS[kind];
      if (!target) throw new Error(`unsupported i18n attribute data-i18n-${kind}`);
      const value = resolve(key, null);
      if (value !== null) result = setAttr(result, target, escapeHtml(value));
    }
    return result;
  });
  out = out.replace(/\sdata-i18n(?:-[\w-]+)?="[^"]*"/g, '');

  if (missing.size) {
    throw new Error(`missing translations: ${[...missing].sort().join(', ')}`);
  }
  return out;
}

function renderNav(page, alternate, t) {
  const items = NAV_ITEMS.map(item => {
    const current = item.page === page;
    return `        <li><a href="${pageHref(item.page)}" class="www-nav-link${current ? ' active' : ''}"${current ? ' aria-current="page"' : ''}><svg class="www-nav-icon" viewBox="0 0 24 24" aria-hidden="true">${ICONS[item.icon]}</svg><span>${t(item.key)}</span></a></li>`;
  }).join('\n');

  return `<nav class="www-nav" aria-label="${t('www.nav.label')}">
    <div class="www-nav-container">
      <a href="./" class="www-nav-logo" aria-label="ccLoad">
        <img class="www-logo-icon" src="brand-mark.svg" alt="" width="36" height="36">
        <svg class="www-logo-wordmark" viewBox="0 0 132 36" aria-hidden="true"><use href="brand-wordmark.svg#brand-wordmark" width="132" height="36"></use></svg>
      </a>
      <ul class="www-nav-menu" id="www-nav-menu">
${items}
      </ul>
      <div class="www-nav-actions">
        <a href="${GITHUB_URL}" target="_blank" rel="noopener" class="www-btn-secondary www-icon-button www-github-button" aria-label="GitHub" title="GitHub"><svg class="www-action-icon" viewBox="0 0 24 24" aria-hidden="true">${GITHUB_ICON}</svg></a>
        <a href="${alternate.href}" hreflang="${alternate.locale.code}" lang="${alternate.locale.code}" data-locale="${alternate.locale.code}" id="www-lang-switch" class="www-btn-secondary www-icon-button www-lang-button" title="${t('www.nav.switchLanguage')}"><svg class="www-action-icon" viewBox="0 0 24 24" aria-hidden="true">${LANG_ICON}</svg><span>${alternate.locale.label}</span></a>
        <button type="button" id="www-theme-switch" class="www-btn-secondary www-icon-button" aria-label="${t('www.nav.switchTheme')}" title="${t('www.nav.switchTheme')}"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><rect x="2" y="3" width="20" height="14" rx="2" ry="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg></button>
        <button type="button" class="www-nav-toggle" id="www-nav-toggle" aria-label="${t('www.nav.toggleMenu')}" aria-controls="www-nav-menu" aria-expanded="false"><svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><line x1="3" y1="12" x2="21" y2="12"/><line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="18" x2="21" y2="18"/></svg></button>
      </div>
    </div>
  </nav>`;
}

function renderFooter(t) {
  const docs = NAV_ITEMS.map(item => `<li><a class="www-footer-link" href="${pageHref(item.page)}">${t(item.key)}</a></li>`).join('');
  const external = [
    [GITHUB_URL, 'www.footer.source'],
    [`${GITHUB_URL}/releases`, 'www.footer.releases'],
    [`${GITHUB_URL}/pkgs/container/ccload`, 'www.footer.image'],
    [`${GITHUB_URL}/issues`, 'www.footer.issues']
  ].map(([href, key]) => `<li><a class="www-footer-link" href="${href}" target="_blank" rel="noopener">${t(key)}</a></li>`).join('');

  return `<footer class="www-footer">
    <div class="www-footer-content">
      <div class="www-footer-section">
        <p class="www-footer-title">ccLoad</p>
        <p class="www-footer-tagline">${t('www.footer.tagline')}</p>
      </div>
      <div class="www-footer-section">
        <p class="www-footer-title">${t('www.footer.docs')}</p>
        <ul>${docs}</ul>
      </div>
      <div class="www-footer-section">
        <p class="www-footer-title">${t('www.footer.project')}</p>
        <ul>${external}</ul>
      </div>
    </div>
    <div class="www-footer-bottom">
      <p>© 2025 ccLoad · <a href="${GITHUB_URL}/blob/master/LICENSE" target="_blank" rel="noopener">MIT License</a></p>
    </div>
  </footer>`;
}

function extractFaq(html) {
  return [...html.matchAll(/<details class="www-faq-item"[^>]*>\s*<summary[^>]*>([\s\S]*?)<\/summary>([\s\S]*?)<\/details>/g)]
    .map(([, question, answer]) => ({
      '@type': 'Question',
      name: decodeText(question),
      acceptedAnswer: { '@type': 'Answer', text: decodeText(answer) }
    }));
}

function renderJsonLd(page, locale, html, meta) {
  const graph = [];
  if (page === 'index') {
    graph.push({
      '@type': 'WebSite',
      '@id': `${SITE_URL}/#website`,
      name: 'ccLoad',
      url: `${SITE_URL}/`,
      inLanguage: ['en', 'zh-CN']
    });
    graph.push({
      '@type': 'SoftwareApplication',
      name: 'ccLoad',
      url: pageUrl(page, locale),
      description: meta.description,
      applicationCategory: 'DeveloperApplication',
      operatingSystem: 'Linux, macOS, Windows',
      license: `${GITHUB_URL}/blob/master/LICENSE`,
      image: meta.image,
      sameAs: [GITHUB_URL],
      offers: { '@type': 'Offer', price: '0', priceCurrency: 'USD' }
    });
    const faq = extractFaq(html);
    if (faq.length) graph.push({ '@type': 'FAQPage', mainEntity: faq });
  } else {
    graph.push({
      '@type': 'BreadcrumbList',
      itemListElement: [
        { '@type': 'ListItem', position: 1, name: 'ccLoad', item: pageUrl('index', locale) },
        { '@type': 'ListItem', position: 2, name: meta.heading, item: pageUrl(page, locale) }
      ]
    });
  }
  // JSON 嵌入 <script> 时转义 <，避免文案中出现 </script>
  const json = JSON.stringify({ '@context': 'https://schema.org', '@graph': graph }).replace(/</g, '\\u003c');
  return `<script type="application/ld+json">${json}</script>`;
}

function renderHead(page, locale, html) {
  const title = decodeText(html.match(/<title>([\s\S]*?)<\/title>/)?.[1] || '');
  const description = html.match(/<meta name="description"[^>]*\scontent="([^"]*)"/)?.[1] || '';
  const heading = decodeText(html.match(/<h1\b[^>]*>([\s\S]*?)<\/h1>/)?.[1] || title);
  if (!title || !description) throw new Error(`${page}.html (${locale.code}) needs <title> and meta description`);

  const image = `${SITE_URL}/assets/images/ccload-promo.${locale.code === 'en' ? 'en' : 'zh-CN'}.jpg`;
  const alternates = LOCALES.map(l => `<link rel="alternate" hreflang="${l.code}" href="${pageUrl(page, l)}">`);
  alternates.push(`<link rel="alternate" hreflang="x-default" href="${pageUrl(page, LOCALES[0])}">`);
  const og = [
    ['og:type', 'website'], ['og:site_name', 'ccLoad'], ['og:title', escapeHtml(title)],
    ['og:description', description], ['og:url', pageUrl(page, locale)], ['og:image', image],
    ['og:image:width', '1280'], ['og:image:height', '720'], ['og:locale', locale.ogLocale],
    ...LOCALES.filter(l => l !== locale).map(l => ['og:locale:alternate', l.ogLocale])
  ].map(([property, content]) => `<meta property="${property}" content="${content}">`);

  return [
    `<link rel="canonical" href="${pageUrl(page, locale)}">`,
    ...alternates,
    '<meta name="robots" content="index, follow, max-image-preview:large">',
    '<meta name="theme-color" content="#050c1c">',
    ...og,
    '<meta name="twitter:card" content="summary_large_image">',
    `<meta name="twitter:title" content="${escapeHtml(title)}">`,
    `<meta name="twitter:description" content="${description}">`,
    `<meta name="twitter:image" content="${image}">`,
    renderJsonLd(page, locale, html, { description: decodeText(description), image, heading })
  ].map(line => `  ${line}`).join('\n');
}

// 已显式选过语言的访客回到其选择；英文页对未选择过的中文浏览器同样跳转。爬虫没有 localStorage，不受影响。
function renderLocaleRedirect(locale, alternate) {
  const detect = locale.code === 'en' ? "||(/^zh\\b/i.test(navigator.language)?'zh-CN':'')" : '';
  return `  <script>try{var l=localStorage.getItem('ccload_locale')${detect};if(l==='${alternate.locale.code}')location.replace('${alternate.href}'+location.hash)}catch(e){}</script>`;
}

function prefixAssets(html, prefix) {
  if (!prefix) return html;
  return html.replace(/(\s(?:src|href|poster)=")([^"]+)"/g, (whole, head, url) => ASSET_URL.test(url) ? `${head}${prefix}${url}"` : whole);
}

export function renderPage(source, page, locale, messages) {
  const other = LOCALES.find(l => l !== locale);
  const alternate = { locale: other, href: localeHref(locale, other, page) };
  const dict = messages[locale.code] || {};
  const t = key => {
    if (dict[key] === undefined) throw new Error(`missing ${locale.code} translation: ${key}`);
    return escapeHtml(dict[key]);
  };

  let html = source
    .replace(/(<body\b[^>]*>\n)/, `$1  ${renderNav(page, alternate, t)}\n`)
    .replace(/(\n)<\/body>/, `$1  ${renderFooter(t)}\n</body>`)
    .replace(/<html lang="[^"]*">/, `<html lang="${locale.code}">`);

  html = translate(html, key => dict[key], { strict: locale.code !== 'en' });
  html = html.replace(/\shref="index\.html(#[^"]*)?"/g, (_, hash = '') => ` href="./${hash}"`);
  html = html.replace(/(<meta name="viewport"[^>]*>\n)/, `$1${renderLocaleRedirect(locale, alternate)}\n`);
  html = html.replace(/(\n)(\s*<\/head>)/, `$1${renderHead(page, locale, html)}\n$2`);
  return prefixAssets(html, locale.dir ? '../' : '');
}

export function siteUrls() {
  return PAGES.flatMap(page => LOCALES.map(locale => pageUrl(page, locale)));
}

function renderSitemap() {
  const urls = PAGES.flatMap(page => LOCALES.map(locale => {
    const links = LOCALES.map(l => `    <xhtml:link rel="alternate" hreflang="${l.code}" href="${pageUrl(page, l)}"/>`).join('\n');
    return `  <url>\n    <loc>${pageUrl(page, locale)}</loc>\n${links}\n  </url>`;
  }));
  return `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">\n${urls.join('\n')}\n</urlset>\n`;
}

export function build({ outDir = path.join(SRC_DIR, 'dist') } = {}) {
  const messages = loadMessages();

  fs.rmSync(outDir, { recursive: true, force: true });
  for (const locale of LOCALES) {
    fs.mkdirSync(path.join(outDir, locale.dir), { recursive: true });
    for (const page of PAGES) {
      const source = fs.readFileSync(path.join(SRC_DIR, `${page}.html`), 'utf8');
      fs.writeFileSync(path.join(outDir, locale.dir, `${page}.html`), renderPage(source, page, locale, messages));
    }
  }
  for (const file of STATIC_FILES) {
    const from = path.join(SRC_DIR, file);
    if (!fs.existsSync(from)) throw new Error(`missing ${file}; run make www-setup first`);
    fs.copyFileSync(from, path.join(outDir, file));
  }
  for (const dir of STATIC_DIRS) {
    const from = path.join(SRC_DIR, dir);
    if (fs.existsSync(from)) {
      fs.cpSync(from, path.join(outDir, dir), { recursive: true, filter: src => !path.basename(src).startsWith('.') });
    }
  }
  fs.writeFileSync(path.join(outDir, 'sitemap.xml'), renderSitemap());
  fs.writeFileSync(path.join(outDir, 'robots.txt'), `User-agent: *\nAllow: /\n\nSitemap: ${SITE_URL}/sitemap.xml\n`);
  fs.writeFileSync(path.join(outDir, `${INDEXNOW_KEY}.txt`), INDEXNOW_KEY);
  return outDir;
}

export async function submitIndexNow() {
  const res = await fetch('https://api.indexnow.org/indexnow', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
    body: JSON.stringify({
      host: new URL(SITE_URL).host,
      key: INDEXNOW_KEY,
      keyLocation: `${SITE_URL}/${INDEXNOW_KEY}.txt`,
      urlList: siteUrls()
    })
  });
  if (!res.ok) throw new Error(`IndexNow HTTP ${res.status}: ${await res.text()}`);
  return res.status;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url) && process.argv[2] === '--indexnow') {
  const status = await submitIndexNow();
  console.log(`✓ IndexNow submitted ${siteUrls().length} URLs (HTTP ${status})`);
} else if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const out = build({ outDir: process.argv[2] ? path.resolve(process.argv[2]) : undefined });
  console.log(`✓ www built: ${out}`);
}
