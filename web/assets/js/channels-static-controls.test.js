const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'channels-init.js'), 'utf8');

test('channels 页固定控件不再使用静态 inline 事件', () => {
  assert.doesNotMatch(html, /onclick="(?:showAddModal|batchEnableSelectedChannels|batchDisableSelectedChannels|batchRefreshSelectedChannelsMerge|batchRefreshSelectedChannelsReplace|clearSelectedChannels|closeModal|openKeyImportModal|openKeyExportModal|toggleInlineKeyVisibility|batchDeleteSelectedKeys|addCommonModels|fetchModelsFromAPI|addRedirectRow|batchLowercaseSelectedModels|batchDeleteSelectedModels|closeDeleteModal|confirmDelete|closeTestModal|runChannelTest|runBatchTest|closeKeyImportModal|confirmInlineKeyImport|closeKeyExportModal|copyExportKeys|downloadExportKeys|closeModelImportModal|confirmModelImport|closeSortModal|saveSortOrder)\(\)"/);
  assert.doesNotMatch(html, /onsubmit="saveChannel\(event\)"/);
  assert.doesNotMatch(html, /onchange="(?:updateTestURL|updateExportPreview)\(\)"/);
  assert.doesNotMatch(html, /onchange="(?:toggleSelectAllURLs|toggleSelectAllKeys|filterKeysByStatus|toggleSelectAllModels)\([^"]*\)"/);
  assert.doesNotMatch(html, /oninput="filterModelsByKeyword\(this\.value\)"/);
  assert.doesNotMatch(html, /onmouseover=|onmouseout=/);
  assert.match(html, /data-action="show-add-modal"/);
  assert.match(html, /data-action="batch-enable-channels"/);
  assert.match(html, /data-action="open-key-import-modal"/);
  assert.match(html, /data-action="close-key-export-modal"/);
  assert.match(html, /data-action="save-sort-order"/);
  assert.match(html, /data-change-action="toggle-select-all-urls"/);
  assert.match(html, /data-change-action="update-test-url"/);
  assert.match(html, /data-change-action="update-export-preview"/);
  assert.match(html, /data-input-action="filter-models-by-keyword"/);
});

test('channels-init.js 使用集中绑定处理固定控件动作', () => {
  assert.match(script, /function initChannelsPageActions\(\)/);
  assert.match(script, /window\.initDelegatedActions\(\{/);
  assert.match(script, /boundKey:\s*'channelsPageActionsBound'/);
  assert.match(script, /'show-add-modal':\s*\(\)\s*=> showAddModal\(\)/);
  assert.match(script, /'batch-enable-channels':\s*\(\)\s*=> batchEnableSelectedChannels\(\)/);
  assert.match(script, /'open-key-import-modal':\s*\(\)\s*=> openKeyImportModal\(\)/);
  assert.match(script, /'close-test-modal':\s*\(\)\s*=> closeTestModal\(\)/);
  assert.match(script, /'save-sort-order':\s*\(\)\s*=> saveSortOrder\(\)/);
  assert.match(script, /'toggle-select-all-urls':\s*\(actionTarget\)\s*=> toggleSelectAllURLs\(actionTarget\.checked\)/);
  assert.match(script, /'update-test-url':\s*\(\)\s*=> updateTestURL\(\)/);
  assert.match(script, /'update-export-preview':\s*\(\)\s*=> updateExportPreview\(\)/);
  assert.match(script, /'filter-models-by-keyword':\s*\(actionTarget\)\s*=> filterModelsByKeyword\(actionTarget\.value\)/);
  assert.match(script, /channelForm\.addEventListener\('submit', \(event\) => \{/);
  assert.match(script, /saveChannel\(event\);/);
  assert.match(script, /initChannelsPageActions\(\);/);
});
