const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const channelsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const channelsUrlsScript = fs.readFileSync(path.join(__dirname, 'channels-urls.js'), 'utf8');
const channelsModalsScript = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

test('channels 添加弹窗在 URL 区域内提前提示重复渠道', () => {
  assert.match(channelsHtml, /id="channelDuplicateHint" class="channel-duplicate-hint" hidden role="status" aria-live="polite"/);
  assert.match(channelsUrlsScript, /scheduleChannelDuplicateHintCheck\(\);/);
  assert.match(channelsModalsScript, /function scheduleChannelDuplicateHintCheck\(\)/);
  assert.match(channelsModalsScript, /function renderChannelDuplicateHint\(dupes\)/);
  assert.match(channelsModalsScript, /channelTypeRadios\.addEventListener\('change', \(event\) => \{[\s\S]*scheduleChannelDuplicateHintCheck\(\);/);
});

test('URL 输入变更会同步 exact URL 对转换方式的限制', () => {
  assert.match(channelsModalsScript, /function syncProtocolTransformModeForURLs\(\)/);
  assert.match(channelsUrlsScript, /function updateInlineURL\(index, value\)[\s\S]*syncProtocolTransformModeForURLs\(\);/);
  assert.match(channelsUrlsScript, /function renderInlineURLTable\(\)[\s\S]*syncProtocolTransformModeForURLs\(\);/);
});
