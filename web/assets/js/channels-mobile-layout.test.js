const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');

function extractBalancedBlock(source, openBraceIndex) {
  let depth = 0;
  for (let i = openBraceIndex; i < source.length; i += 1) {
    if (source[i] === '{') depth += 1;
    if (source[i] === '}') {
      depth -= 1;
      if (depth === 0) return source.slice(openBraceIndex + 1, i);
    }
  }
  throw new Error('Unclosed CSS block');
}

function mobileChannelTableCss(source) {
  const mediaStart = '@media (max-width: 768px)';
  let cursor = 0;
  while ((cursor = source.indexOf(mediaStart, cursor)) !== -1) {
    const openBrace = source.indexOf('{', cursor + mediaStart.length);
    const block = extractBalancedBlock(source, openBrace);
    if (block.includes('.channel-table .ch-col-last-success')) return block;
    cursor = openBrace + block.length + 2;
  }
  throw new Error('Mobile channel table media block not found');
}

test('mobile last-request summary and detail panel stay inside the channel card viewport', () => {
  const mobileCss = mobileChannelTableCss(css);

  assert.match(
    mobileCss,
    /\.channel-table \.ch-col-last-success\s*{[^}]*flex-wrap:\s*wrap;/s,
    'the last-success row must allow the failure summary to move to a second line',
  );
  assert.match(
    mobileCss,
    /\.channel-table \.ch-col-last-success \.ch-last-request-slot\s*{[^}]*flex:\s*1 0 100%;[^}]*width:\s*100%;[^}]*max-width:\s*100%;[^}]*margin-left:\s*0;/s,
    'the failure summary slot must use a full-width mobile row',
  );
  assert.match(
    mobileCss,
    /\.channel-table \.ch-last-request__panel\s*{[^}]*left:\s*auto;[^}]*right:\s*0;[^}]*max-width:\s*calc\(100vw - 48px\);/s,
    'the expanded detail panel must open toward the viewport interior',
  );
});
