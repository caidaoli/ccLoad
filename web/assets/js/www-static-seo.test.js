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
