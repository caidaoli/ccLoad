const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'theme-init.js'), 'utf8');

function memoryStorage() {
  const values = new Map();
  return {
    getItem(key) { return values.get(key) ?? null; },
    setItem(key, value) { values.set(key, String(value)); }
  };
}

function cookieJar() {
  const values = new Map();
  return {
    get value() {
      return [...values].map(([key, value]) => `${key}=${value}`).join('; ');
    },
    set value(raw) {
      const [pair] = String(raw).split(';');
      const separator = pair.indexOf('=');
      values.set(pair.slice(0, separator), pair.slice(separator + 1));
    }
  };
}

function loadTheme(storage, cookies, prefersDark = false) {
  const style = {
    colorScheme: '',
    backgroundColor: '',
    color: '',
    removeProperty() {}
  };
  const document = {
    documentElement: { dataset: {}, style },
    readyState: 'loading',
    querySelector() { return null; },
    addEventListener() {},
    get cookie() { return cookies.value; },
    set cookie(value) { cookies.value = value; }
  };
  const window = {
    matchMedia() { return { matches: prefersDark }; }
  };

  vm.runInNewContext(source, {
    window,
    document,
    localStorage: storage,
    requestAnimationFrame() {}
  });
  return { window, document };
}

test('explicit theme survives a page reload', () => {
  const storage = memoryStorage();
  const cookies = cookieJar();
  const firstPage = loadTheme(storage, cookies);

  assert.equal(firstPage.window.ccLoadTheme.setStoredTheme('dark'), true);

  const reloadedPage = loadTheme(storage, cookies);
  assert.equal(reloadedPage.document.documentElement.dataset.theme, 'dark');
  assert.equal(reloadedPage.document.documentElement.dataset.resolvedTheme, 'dark');
});

test('cookie preserves the theme when Edge-style browser storage is unavailable', () => {
  const blockedStorage = {
    getItem() { throw new Error('storage blocked'); },
    setItem() { throw new Error('storage blocked'); }
  };
  const cookies = cookieJar();
  const firstPage = loadTheme(blockedStorage, cookies);

  assert.equal(firstPage.window.ccLoadTheme.setStoredTheme('dark'), true);

  const reloadedPage = loadTheme(blockedStorage, cookies);
  assert.equal(reloadedPage.document.documentElement.dataset.theme, 'dark');
  assert.equal(reloadedPage.document.documentElement.dataset.resolvedTheme, 'dark');
});

test('local storage preserves the newest theme when the cookie is readable but not writable', () => {
  const storage = memoryStorage();
  const cookies = {
    get value() { return 'ccload_theme=dark'; },
    set value(_) {}
  };
  const firstPage = loadTheme(storage, cookies);

  assert.equal(firstPage.document.documentElement.dataset.theme, 'dark');
  assert.equal(firstPage.window.ccLoadTheme.setStoredTheme('light'), true);

  const reloadedPage = loadTheme(storage, cookies);
  assert.equal(reloadedPage.document.documentElement.dataset.theme, 'light');
  assert.equal(reloadedPage.document.documentElement.dataset.resolvedTheme, 'light');
});

test('cookie preserves the newest theme when local storage is readable but not writable', () => {
  const storage = {
    getItem() { return 'light'; },
    setItem() { throw new Error('storage is read-only'); }
  };
  const cookies = cookieJar();
  const firstPage = loadTheme(storage, cookies);

  assert.equal(firstPage.document.documentElement.dataset.theme, 'light');
  assert.equal(firstPage.window.ccLoadTheme.setStoredTheme('dark'), true);

  const reloadedPage = loadTheme(storage, cookies);
  assert.equal(reloadedPage.document.documentElement.dataset.theme, 'dark');
  assert.equal(reloadedPage.document.documentElement.dataset.resolvedTheme, 'dark');
});
