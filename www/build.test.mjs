import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { build, translate, PAGES, LOCALES, SITE_URL, INDEXNOW_KEY } from './build.mjs';

const outDir = build({ outDir: fs.mkdtempSync(path.join(os.tmpdir(), 'ccload-www-')) });
test.after(() => fs.rmSync(outDir, { recursive: true, force: true }));

const pages = PAGES.flatMap(page => LOCALES.map(locale => {
  const rel = `${locale.dir}${page}.html`;
  return { page, locale, rel, html: fs.readFileSync(path.join(outDir, rel), 'utf8') };
}));

function canonicalOf(html) {
  return html.match(/<link rel="canonical" href="([^"]+)">/)?.[1];
}

test('每个页面按语言预渲染，不残留运行时 i18n', () => {
  for (const { rel, locale, html } of pages) {
    assert.match(html, new RegExp(`<html lang="${locale.code}">`), rel);
    assert.doesNotMatch(html, /data-i18n/, rel);
    assert.doesNotMatch(html, /assets\/locales\/|i18n\.js/, rel);
    assert.match(html, /<nav class="www-nav"/, rel);
    assert.match(html, /<footer class="www-footer"/, rel);
  }
  const zhHome = pages.find(p => p.page === 'index' && p.locale.code === 'zh-CN').html;
  assert.match(zhHome, /<h1 class="www-hero-title">.*让 AI 编程.*为 Claude Code、Codex、Gemini 打造的自托管 API 网关.*<\/h1>/);
  assert.match(zhHome, /src="\.\.\/assets\/video\/ccload-promo\.zh-CN\.mp4/);
});

test('站内引用的本地资源和页面都存在', () => {
  for (const { rel, html } of pages) {
    const base = path.dirname(path.join(outDir, rel));
    for (const [, url] of html.matchAll(/\s(?:src|href|poster)="([^"]+)"/g)) {
      if (/^(?:[a-z]+:|#)/i.test(url)) continue;
      // 介绍视频不入库（CI 中不存在），由 make www-release 发布前单独校验
      if (url.includes('assets/video/')) continue;
      let target = path.resolve(base, url.split(/[?#]/)[0]);
      if (url.split(/[?#]/)[0].endsWith('/') || fs.statSync(target, { throwIfNoEntry: false })?.isDirectory()) {
        target = path.join(target, 'index.html');
      }
      assert.ok(fs.existsSync(target), `${rel} -> ${url}`);
    }
  }
});

test('canonical、hreflang 与 sitemap 一致且互相指向', () => {
  const canonicals = new Set(pages.map(({ html }) => canonicalOf(html)));
  assert.equal(canonicals.size, pages.length);
  for (const { rel, html } of pages) {
    const alternates = Object.fromEntries([...html.matchAll(/<link rel="alternate" hreflang="([^"]+)" href="([^"]+)">/g)].map(m => [m[1], m[2]]));
    for (const locale of LOCALES) assert.ok(canonicals.has(alternates[locale.code]), `${rel} hreflang ${locale.code}`);
    assert.equal(alternates['x-default'], alternates.en, rel);
    assert.ok(canonicalOf(html).startsWith(`${SITE_URL}/`), rel);
  }

  const sitemap = fs.readFileSync(path.join(outDir, 'sitemap.xml'), 'utf8');
  const locs = new Set([...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map(m => m[1]));
  assert.deepEqual([...locs].sort(), [...canonicals].sort());
  assert.match(fs.readFileSync(path.join(outDir, 'robots.txt'), 'utf8'), new RegExp(`Sitemap: ${SITE_URL}/sitemap.xml`));
  assert.equal(fs.readFileSync(path.join(outDir, `${INDEXNOW_KEY}.txt`), 'utf8'), INDEXNOW_KEY);
});

test('首页 JSON-LD 可解析，FAQ 与页面问答一一对应', () => {
  for (const { page, rel, html } of pages.filter(p => p.page === 'index')) {
    const data = JSON.parse(html.match(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/)[1]);
    const types = data['@graph'].map(node => node['@type']);
    assert.deepEqual(types, ['WebSite', 'SoftwareApplication', 'FAQPage'], `${page} ${rel}`);
    const faq = data['@graph'][2].mainEntity;
    assert.equal(faq.length, (html.match(/<details class="www-faq-item"/g) || []).length, rel);
    for (const item of faq) {
      assert.ok(item.name && item.acceptedAnswer.text && !/[<>]/.test(item.acceptedAnswer.text), rel);
    }
  }
});

test('translate 处理同名嵌套、转义纯文本，并在缺译文时失败', () => {
  const dict = { a: '<b>粗体</b>', t: 'A & B', alt: '图 "1"' };
  const html = '<p data-i18n-html="a"><span><span>x</span></span></p><span data-i18n="t">x</span><img data-i18n-alt="alt" alt="x">';
  assert.equal(
    translate(html, key => dict[key], { strict: true }),
    '<p><b>粗体</b></p><span>A &amp; B</span><img alt="图 &quot;1&quot;">'
  );
  assert.equal(translate('<div data-i18n="nope">Source</div>', () => undefined, { strict: false }), '<div>Source</div>');
  assert.throws(() => translate('<div data-i18n="nope">x</div>', () => undefined, { strict: true }), /missing translations: nope/);
});
