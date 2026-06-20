const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const repoRoot = path.join(__dirname, '..', '..', '..');
const wwwDir = path.join(repoRoot, 'www');
const hanPattern = /[\u3400-\u9fff]/u;

function read(filePath) {
  return fs.readFileSync(filePath, 'utf8');
}

test('www static HTML defaults to English for SEO', () => {
  const htmlFiles = fs.readdirSync(wwwDir)
    .filter((name) => name.endsWith('.html'))
    .sort();

  assert.ok(htmlFiles.length > 0, 'expected www html files');

  for (const fileName of htmlFiles) {
    const filePath = path.join(wwwDir, fileName);
    const html = read(filePath);

    assert.match(html, /<html lang="en">/, `${fileName} should default to lang=en`);
    assert.doesNotMatch(html, hanPattern, `${fileName} should not contain Chinese fallback text`);
  }
});

test('www i18n supports SEO-relevant attributes', () => {
  const source = read(path.join(wwwDir, 'assets', 'js', 'i18n.js'));

  assert.match(source, /\[data-i18n-content\]/, 'meta content translation support is required');
  assert.match(source, /\[data-i18n-alt\]/, 'image alt translation support is required');
  assert.match(source, /\[data-i18n-aria-label\]/, 'aria-label translation support is required');
});

test('www documents recently added operational features', () => {
  const index = read(path.join(wwwDir, 'index.html'));
  const config = read(path.join(wwwDir, 'config.html'));
  const usage = read(path.join(wwwDir, 'usage.html'));
  const en = read(path.join(wwwDir, 'assets', 'locales', 'en.js'));
  const zh = read(path.join(wwwDir, 'assets', 'locales', 'zh-CN.js'));

  const featureKeys = ['proxy', 'dns', 'quota'];
  for (const key of featureKeys) {
    assert.match(index, new RegExp(`www\\.home\\.features\\.${key}\\.title`), `${key} feature card is missing`);
    assert.match(en, new RegExp(`www\\.home\\.features\\.${key}\\.title`), `${key} English title is missing`);
    assert.match(zh, new RegExp(`www\\.home\\.features\\.${key}\\.title`), `${key} Chinese title is missing`);
  }

  assert.match(config, /CCLOAD_HOST_OVERRIDES/, 'DNS host override env var is missing');
  assert.match(config, /Invalid entries fail startup/, 'DNS host override fail-fast behavior is missing');
  assert.match(config, /proxy_url/, 'channel-level proxy field is missing');
  assert.match(config, /socks5h/, 'SOCKS5 hostname-resolving proxy support is missing');
  assert.match(usage, /UI timeline success rate includes 429/, '429 UI success-rate scope is missing');
  assert.match(usage, /health-score sorting excludes 429/, '429 health-score sorting scope is missing');
  assert.doesNotMatch(usage, /429 is excluded from health success rate/, 'stale 429 success-rate wording must be removed');
});
