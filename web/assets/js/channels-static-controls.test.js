const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const initScript = fs.readFileSync(path.join(__dirname, 'channels-init.js'), 'utf8');
const modalsScript = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

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
  assert.match(initScript, /function initChannelsPageActions\(\)/);
  assert.match(initScript, /typeof initChannelEditorActions === 'function'/);
  assert.match(initScript, /initChannelEditorActions\(\);/);
  assert.match(initScript, /window\.initDelegatedActions\(\{/);
  assert.match(initScript, /boundKey:\s*'channelsPageActionsBound'/);
  assert.match(initScript, /'show-add-modal':\s*\(\)\s*=> showAddModal\(\)/);
  assert.match(initScript, /'batch-enable-channels':\s*\(\)\s*=> batchEnableSelectedChannels\(\)/);
  assert.match(initScript, /'close-test-modal':\s*\(\)\s*=> closeTestModal\(\)/);
  assert.match(initScript, /'save-sort-order':\s*\(\)\s*=> saveSortOrder\(\)/);
  assert.match(initScript, /'update-test-url':\s*\(\)\s*=> updateTestURL\(\)/);
  assert.doesNotMatch(initScript, /'open-key-import-modal':\s*\(\)\s*=> openKeyImportModal\(\)/);
  assert.doesNotMatch(initScript, /'toggle-select-all-urls':\s*\(actionTarget\)\s*=> toggleSelectAllURLs\(actionTarget\.checked\)/);
  assert.doesNotMatch(initScript, /'update-export-preview':\s*\(\)\s*=> updateExportPreview\(\)/);
  assert.match(initScript, /initChannelsPageActions\(\);/);
});

test('channels-modals.js 负责渠道编辑器弹窗固定动作与表单提交绑定', () => {
  assert.match(modalsScript, /function initChannelEditorActions\(\)/);
  assert.match(modalsScript, /boundKey:\s*'channelEditorActionsBound'/);
  assert.match(modalsScript, /'open-key-import-modal':\s*\(\)\s*=> invokeChannelEditorAction\('openKeyImportModal'\)/);
  assert.match(modalsScript, /'toggle-select-all-urls':\s*\(actionTarget\)\s*=> invokeChannelEditorAction\('toggleSelectAllURLs', actionTarget\.checked\)/);
  assert.match(modalsScript, /'filter-models-by-keyword':\s*\(actionTarget\)\s*=> invokeChannelEditorAction\('filterModelsByKeyword', actionTarget\.value\)/);
  assert.match(modalsScript, /channelForm\.addEventListener\('submit', \(event\) => \{/);
  assert.match(modalsScript, /channelForm\.dataset\.channelFormBound = '1';/);
  assert.match(modalsScript, /saveChannel\(event\);/);
});
