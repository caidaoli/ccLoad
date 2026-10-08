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
  message: '<path d="M21 12a8 8 0 0 1-8 8H6l-3 3v-7a8 8 0 1 1 18-4Z"/>',
  key: '<circle cx="7.5" cy="15.5" r="5.5"/><path d="m21 2-9.6 9.6"/><path d="m15.5 7.5 3 3L22 7l-3-3"/>',
  shuffle: '<path d="M2 18h1.4c1.3 0 2.5-.6 3.3-1.7l6.1-8.6c.7-1.1 2-1.7 3.3-1.7H22"/><path d="m18 2 4 4-4 4"/><path d="M2 6h1.9c1.5 0 2.9.9 3.6 2.2"/><path d="M22 18h-5.9c-1.3 0-2.6-.7-3.3-1.8l-.5-.8"/><path d="m18 14 4 4-4 4"/>',
  shield: '<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/><path d="m9 12 2 2 4-4"/>',
  'shield-alert': '<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/><path d="M12 8v4"/><path d="M12 16h.01"/>',
  snowflake: '<path d="M2 12h20"/><path d="M12 2v20"/><path d="m20 16-4-4 4-4"/><path d="m4 8 4 4-4 4"/><path d="m16 4-4 4-4-4"/><path d="m8 20 4-4 4 4"/>',
  coins: '<circle cx="8" cy="8" r="6"/><path d="M18.09 10.37A6 6 0 1 1 10.34 18"/><path d="M7 6h1v4"/><path d="m16.71 13.88.7.71-2.82 2.82"/>',
  feather: '<path d="M12.67 19a2 2 0 0 0 1.416-.588l6.154-6.172a6 6 0 0 0-8.49-8.49L5.586 9.914A2 2 0 0 0 5 11.328V18a1 1 0 0 0 1 1z"/><path d="M16 8 2 22"/><path d="M17.5 15H9"/>',
  route: '<circle cx="6" cy="19" r="3"/><path d="M9 19h8.5a3.5 3.5 0 0 0 0-7h-11a3.5 3.5 0 0 1 0-7H15"/><circle cx="18" cy="5" r="3"/>',
  refresh: '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
  globe: '<circle cx="12" cy="12" r="10"/><path d="M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20"/><path d="M2 12h20"/>',
  link: '<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>',
  sparkles: '<path d="M12 3l1.9 5.8L20 11l-6.1 2.2L12 19l-1.9-5.8L4 11l6.1-2.2z"/><path d="M19 3v4"/><path d="M21 5h-4"/>',
  zap: '<path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/>',
  'list-check': '<path d="M11 18H3"/><path d="m15 18 2 2 4-4"/><path d="M16 12H3"/><path d="M16 6H3"/>',
  chart: '<path d="M3 3v16a2 2 0 0 0 2 2h16"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/>',
  asterisk: '<path d="M12 6v12"/><path d="M17.196 9 6.804 15"/><path d="m6.804 9 10.392 6"/>',
  cloud: '<path d="M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9Z"/>',
  bot: '<path d="M12 8V4H8"/><rect width="16" height="12" x="4" y="8" rx="2"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/>',
  code: '<path d="m16 18 6-6-6-6"/><path d="m8 6-6 6 6 6"/>',
  pointer: '<path d="M4.037 4.688a.495.495 0 0 1 .651-.651l16 6.5a.5.5 0 0 1-.063.947l-6.124 1.58a2 2 0 0 0-1.438 1.435l-1.579 6.126a.5.5 0 0 1-.947.063z"/>',
  keyboard: '<rect width="20" height="16" x="2" y="4" rx="2"/><path d="M6 8h.01"/><path d="M10 8h.01"/><path d="M14 8h.01"/><path d="M18 8h.01"/><path d="M8 12h.01"/><path d="M12 12h.01"/><path d="M16 12h.01"/><path d="M7 16h10"/>',
  layers: '<path d="M12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83Z"/><path d="m22 17.65-9.17 4.16a2 2 0 0 1-1.66 0L2 17.65"/><path d="m22 12.65-9.17 4.16a2 2 0 0 1-1.66 0L2 12.65"/>',
  smile: '<circle cx="12" cy="12" r="10"/><path d="M8 14s1.5 2 4 2 4-2 4-2"/><path d="M9 9h.01"/><path d="M15 9h.01"/>',
  wrench: '<path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/>',
  mug: '<path d="M17 11h1a3 3 0 0 1 0 6h-1"/><path d="M5 8h12v9a4 4 0 0 1-4 4H9a4 4 0 0 1-4-4z"/><path d="M8 2v3"/><path d="M12 2v3"/>',
  package: '<path d="M11 21.73a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73z"/><path d="M12 22V12"/><path d="m3.3 7 8.7 5 8.7-5"/><path d="m7.5 4.27 9 5.15"/>',
  clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
  plug: '<path d="M12 22v-5"/><path d="M9 8V2"/><path d="M15 8V2"/><path d="M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z"/>',
  trending: '<path d="M22 7 13.5 15.5 8.5 10.5 2 17"/><path d="M16 7h6v6"/>',
  card: '<rect width="20" height="14" x="2" y="5" rx="2"/><path d="M2 10h20"/>',
  blocks: '<rect width="7" height="7" x="14" y="3" rx="1"/><path d="M10 21V8a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-5a1 1 0 0 0-1-1H3"/>',
  flask: '<path d="M10 2v7.31"/><path d="M14 9.3V2"/><path d="M8.5 2h7"/><path d="M14 9.3a6.5 6.5 0 1 1-4 0"/><path d="M5.52 16h12.96"/>',
  gauge: '<path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/>',
  image: '<rect width="18" height="18" x="3" y="3" rx="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.09-3.09a2 2 0 0 0-2.82 0L6 21"/>',
  book: '<path d="M4 19.5v-15A2.5 2.5 0 0 1 6.5 2H19a1 1 0 0 1 1 1v18a1 1 0 0 1-1 1H6.5a1 1 0 0 1 0-5H20"/>',
  upgrade: '<circle cx="12" cy="12" r="10"/><path d="m16 12-4-4-4 4"/><path d="M12 16V8"/>',
  bug: '<path d="m8 2 1.88 1.88"/><path d="M14.12 3.88 16 2"/><path d="M9 7.13v-1a3.003 3.003 0 1 1 6 0v1"/><path d="M12 20c-3.3 0-6-2.7-6-6v-3a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v3c0 3.3-2.7 6-6 6"/><path d="M12 20v-9"/><path d="M6.53 9C4.6 8.8 3 7.1 3 5"/><path d="M6 13H2"/><path d="M3 21c0-2.1 1.7-3.9 3.8-4"/><path d="M20.97 5c0 2.1-1.6 3.8-3.5 4"/><path d="M22 13h-4"/><path d="M17.2 17c2.1.1 3.8 1.9 3.8 4"/>',
  lightbulb: '<path d="M15 14c.2-1 .7-1.7 1.5-2.5 1-.9 1.5-2.2 1.5-3.5A6 6 0 0 0 6 8c0 1 .2 2.2 1.5 3.5.7.7 1.3 1.5 1.5 2.5"/><path d="M9 18h6"/><path d="M10 22h4"/>',
  lock: '<rect width="18" height="11" x="3" y="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
  star: '<path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.123 2.123 0 0 0 1.595 1.16l5.166.756a.53.53 0 0 1 .294.904l-3.736 3.638a2.123 2.123 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.122 2.122 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.122 2.122 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.755a2.122 2.122 0 0 0 1.597-1.16z"/>',
  users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>',
  user: '<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>',
  'bar-chart': '<path d="M5 21V12"/><path d="M12 21V4"/><path d="M19 21v-6"/>',
  docker: '<path d="M2 13h17.5c1 0 1.9-.5 2.5-1.4-.7-.4-1.6-.5-2.4-.2-.2-1-.8-1.8-1.6-2.3"/><path d="M2.4 13c.4 4.4 3.7 7 8.4 7 4.4 0 7.5-2.4 8.7-7"/><rect x="4.5" y="9.4" width="2.6" height="2.6" rx=".3"/><rect x="8" y="9.4" width="2.6" height="2.6" rx=".3"/><rect x="11.5" y="9.4" width="2.6" height="2.6" rx=".3"/><rect x="8" y="5.9" width="2.6" height="2.6" rx=".3"/>',
  apple: '<path d="M12 20.94c1.5 0 2.75 1.06 4 1.06 3 0 6-8 6-12.22A4.91 4.91 0 0 0 17 5c-2.22 0-4 1.44-5 2-1-.56-2.78-2-5-2a4.9 4.9 0 0 0-5 4.78C2 14 5 22 8 22c1.25 0 2.5-1.06 4-1.06Z"/><path d="M10 2c1 .5 2 2 2 5"/>',
  laptop: '<path d="M20 16V7a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v9m16 0H4m16 0 1.28 2.55a1 1 0 0 1-.9 1.45H3.62a1 1 0 0 1-.9-1.45L4 16"/>',
  grid: '<rect width="7" height="7" x="3" y="3" rx="1"/><rect width="7" height="7" x="14" y="3" rx="1"/><rect width="7" height="7" x="14" y="14" rx="1"/><rect width="7" height="7" x="3" y="14" rx="1"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  'chevron-right': '<path d="m9 18 6-6-6-6"/>',
  openai: '<g stroke-width="1.6"><rect x="9.25" y="2.75" width="5.5" height="11" rx="2.75"/><rect x="9.25" y="2.75" width="5.5" height="11" rx="2.75" transform="rotate(60 12 12)"/><rect x="9.25" y="2.75" width="5.5" height="11" rx="2.75" transform="rotate(120 12 12)"/><rect x="9.25" y="2.75" width="5.5" height="11" rx="2.75" transform="rotate(180 12 12)"/><rect x="9.25" y="2.75" width="5.5" height="11" rx="2.75" transform="rotate(240 12 12)"/><rect x="9.25" y="2.75" width="5.5" height="11" rx="2.75" transform="rotate(300 12 12)"/></g>',
  play: '<circle cx="12" cy="12" r="10"/><path d="m10 8 6 4-6 4z"/>',
  'arrow-right': '<path d="M5 12h14"/><path d="m13 6 6 6-6 6"/>',
  'check-circle': '<circle cx="12" cy="12" r="10"/><path d="m8 12 3 3 5-6"/>',
  'x-circle': '<circle cx="12" cy="12" r="10"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/>',
  info: '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>',
  'git-branch': '<circle cx="6" cy="5" r="2"/><circle cx="6" cy="19" r="2"/><circle cx="18" cy="8" r="2"/><path d="M6 7v10"/><path d="M18 10c0 4-6 3-11.5 7.5"/>',
  'external-link': '<path d="M7 17 17 7"/><path d="M8 7h9v9"/>',
  server: '<rect width="20" height="8" x="2" y="2" rx="2"/><rect width="20" height="8" x="2" y="14" rx="2"/><path d="M6 6h.01"/><path d="M6 18h.01"/>'
};

const GITHUB_ICON = '<path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/>';
const CHEVRON_ICON = '<path d="m6 9 6 6 6-6"/>';
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
        <a href="${alternate.href}" hreflang="${alternate.locale.code}" lang="${alternate.locale.code}" data-locale="${alternate.locale.code}" id="www-lang-switch" class="www-btn-secondary www-icon-button www-lang-button" title="${t('www.nav.switchLanguage')}"><svg class="www-action-icon" viewBox="0 0 24 24" aria-hidden="true">${LANG_ICON}</svg><span>${alternate.locale.label}</span><svg class="www-action-chevron" viewBox="0 0 24 24" aria-hidden="true">${CHEVRON_ICON}</svg></a>
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
      <p>© ${new Date().getFullYear()} ccLoad · <a href="${GITHUB_URL}/blob/master/LICENSE" target="_blank" rel="noopener">MIT License</a></p>
    </div>
  </footer>`;
}

// 源 HTML 用 <span class="..." data-icon="name"></span> 占位，构建时内联为线性 SVG；未知图标名直接失败
function iconSvg(name) {
  if (!ICONS[name]) throw new Error(`unknown icon: ${name}`);
  return `<svg class="www-i" viewBox="0 0 24 24">${ICONS[name]}</svg>`;
}

function renderIcons(html) {
  return html.replace(/<span(?: class="([^"]+)")? data-icon="([\w-]+)"><\/span>/g, (_, cls, name) =>
    `<span${cls ? ` class="${cls}"` : ''} aria-hidden="true">${iconSvg(name)}</span>`);
}

// 文档页：标题区加面包屑；带 id 的 section 不少于 3 个时，按其 h2 生成左侧本页导航（data-toc-icon 为条目图标）
function renderDocShell(html, page, t) {
  const hero = '<header class="www-page-hero">';
  if (page === 'index' || !html.includes(hero)) return html;
  const current = NAV_ITEMS.find(item => item.page === page);
  const crumb = `<nav class="www-breadcrumb" aria-label="${t('www.docs.breadcrumb')}"><a href="./">ccLoad</a><span aria-hidden="true">/</span><span aria-current="page">${t(current.key)}</span></nav>`;
  html = html.replace(/(<div class="www-page-hero-inner">\n)/, `$1      ${crumb}\n`);

  const sections = [...html.matchAll(/<section\b([^>]*)>[\s\S]*?<h2\b[^>]*>([\s\S]*?)<\/h2>/g)]
    .map(([, attrs, title]) => ({
      id: attrs.match(/\sid="([^"]+)"/)?.[1],
      icon: attrs.match(/\sdata-toc-icon="([\w-]+)"/)?.[1],
      title: escapeHtml(decodeText(title))
    }))
    .filter(s => s.id);
  if (sections.length < 3) return html;

  const links = sections.map(s => `<li><a href="#${s.id}">${s.icon ? `<span aria-hidden="true">${iconSvg(s.icon)}</span>` : ''}<span>${s.title}</span></a></li>`).join('');
  const sidebar = `<aside class="www-doc-sidebar">
    <nav class="www-doc-toc" aria-label="${t('www.docs.onThisPage')}">
      <p class="www-doc-toc-title">${t(current.key)}</p>
      <ul>${links}</ul>
    </nav>
    <a class="www-doc-toc-github" href="${GITHUB_URL}" target="_blank" rel="noopener"><svg viewBox="0 0 24 24" aria-hidden="true">${GITHUB_ICON}</svg><span>${t('www.docs.github')}</span></a>
  </aside>`;
  return html
    .replace(hero, `<div class="www-doc-shell">\n  ${sidebar}\n  <div class="www-doc-body">\n  ${hero}`)
    .replace(/<\/main>/, '</main>\n  </div>\n  </div>');
}

// 代码块左侧行号：独立于 <pre>，复制时不会带上
function renderCodeGutters(html) {
  return html.replace(/(<div class="www-code-content">\s*)(<pre><code[^>]*>([\s\S]*?)<\/code><\/pre>)/g, (_, head, pre, code) => {
    const count = code.split('\n').length;
    if (count < 2) return head + pre;
    const lines = Array.from({ length: count }, (__, i) => i + 1).join('\n');
    return `${head}<span class="www-code-lines" aria-hidden="true">${lines}</span>${pre}`;
  });
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

  let html = renderIcons(source)
    .replace(/(<body\b[^>]*>\n)/, `$1  ${renderNav(page, alternate, t)}\n`)
    .replace(/(\n)<\/body>/, `$1  ${renderFooter(t)}\n</body>`)
    .replace(/<html lang="[^"]*">/, `<html lang="${locale.code}">`);

  html = translate(html, key => dict[key], { strict: locale.code !== 'en' });
  html = renderDocShell(html, page, t);
  html = renderCodeGutters(html);
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
