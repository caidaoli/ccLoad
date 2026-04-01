const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const initScript = fs.readFileSync(path.join(__dirname, 'channels-init.js'), 'utf8');
const modalsScript = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

function sliceSection(source, startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  assert.notEqual(start, -1, `missing section start: ${startMarker}`);

  const end = endMarker ? source.indexOf(endMarker, start) : source.length;
  assert.notEqual(end, -1, `missing section end: ${endMarker}`);

  return source.slice(start, end);
}

test('channels 页固定控件不再使用静态 inline 事件', () => {
  assert.doesNotMatch(html, /onclick="(?:showAddModal|batchEnableSelectedChannels|batchDisableSelectedChannels|batchDeleteSelectedChannels|batchRefreshSelectedChannelsMerge|batchRefreshSelectedChannelsReplace|clearSelectedChannels|closeModal|openKeyImportModal|openKeyExportModal|toggleInlineKeyVisibility|batchDeleteSelectedKeys|addCommonModels|fetchModelsFromAPI|addRedirectRow|batchLowercaseSelectedModels|batchDeleteSelectedModels|closeDeleteModal|confirmDelete|closeTestModal|runChannelTest|runBatchTest|closeKeyImportModal|confirmInlineKeyImport|closeKeyExportModal|copyExportKeys|downloadExportKeys|closeModelImportModal|confirmModelImport|closeSortModal|saveSortOrder)\(\)"/);
  assert.doesNotMatch(html, /onsubmit="saveChannel\(event\)"/);
  assert.doesNotMatch(html, /onchange="(?:updateTestURL|updateExportPreview)\(\)"/);
  assert.doesNotMatch(html, /onchange="(?:toggleSelectAllURLs|toggleSelectAllKeys|filterKeysByStatus|toggleSelectAllModels)\([^"]*\)"/);
  assert.doesNotMatch(html, /oninput="filterModelsByKeyword\(this\.value\)"/);
  assert.doesNotMatch(html, /onmouseover=|onmouseout=/);
  assert.match(html, /data-action="show-add-modal"/);
  assert.match(html, /data-action="batch-enable-channels"/);
  assert.match(html, /data-action="batch-delete-channels"/);
  assert.match(html, /data-action="open-key-import-modal"/);
  assert.match(html, /data-action="close-key-export-modal"/);
  assert.match(html, /data-action="save-sort-order"/);
  assert.match(html, /data-change-action="toggle-select-all-urls"/);
  assert.match(html, /data-change-action="update-test-url"/);
  assert.match(html, /data-change-action="update-export-preview"/);
  assert.match(html, /data-input-action="filter-models-by-keyword"/);
});

test('channels 页 modal 壳层保留 DOM 契约且不再依赖静态 inline 样式', () => {
  const batchProgressSection = sliceSection(html, 'id="batchTestProgress"', '<!-- 测试结果 -->');
  const keyImportSection = sliceSection(html, 'id="keyImportModal"', '<!-- Key导出模态框 -->');
  const keyExportSection = sliceSection(html, 'id="keyExportModal"', '<!-- 模型导入模态框 -->');
  const modelImportSection = sliceSection(html, 'id="modelImportModal"', '<!-- 渠道排序模态框 -->');
  const sortSection = sliceSection(html, 'id="sortModal"', '<!-- HTML模板定义');

  assert.match(batchProgressSection, /id="batchTestProgress" class="channel-batch-progress hidden"[\s\S]*?id="batchTestCounter"[\s\S]*?id="batchTestProgressBar"[\s\S]*?id="batchTestStatus"/);
  assert.match(keyImportSection, /class="modal-content modal-content--md"/);
  assert.match(keyImportSection, /id="keyImportTextarea"/);
  assert.match(keyImportSection, /id="keyImportPreviewContent" class="channel-import-preview-content hidden"/);
  assert.match(keyExportSection, /class="modal-content modal-content--sm"/);
  assert.match(keyExportSection, /name="exportSeparator"/);
  assert.match(keyExportSection, /data-change-action="update-export-preview"/);
  assert.match(keyExportSection, /id="keyExportPreview"/);
  assert.match(modelImportSection, /class="modal-content modal-content--md"/);
  assert.match(modelImportSection, /id="modelImportTextarea"/);
  assert.match(modelImportSection, /id="modelImportPreviewContent" class="channel-import-preview-content hidden"/);
  assert.match(sortSection, /class="modal-content modal-content--xl modal-content--tall channel-sort-modal"/);
  assert.match(sortSection, /id="sortListContainer"/);

  assert.doesNotMatch(batchProgressSection, /style=/);
  assert.doesNotMatch(keyImportSection, /style=/);
  assert.doesNotMatch(keyExportSection, /style=/);
  assert.doesNotMatch(modelImportSection, /style=/);
  assert.doesNotMatch(sortSection, /style=/);
});

test('channels-init.js 使用集中绑定处理固定控件动作', () => {
  assert.match(initScript, /function initChannelsPageActions\(\)/);
  assert.match(initScript, /typeof initChannelEditorActions === 'function'/);
  assert.match(initScript, /initChannelEditorActions\(\);/);
  assert.match(initScript, /window\.initDelegatedActions\(\{/);
  assert.match(initScript, /boundKey:\s*'channelsPageActionsBound'/);
  assert.match(initScript, /'show-add-modal':\s*\(\)\s*=> showAddModal\(\)/);
  assert.match(initScript, /'batch-enable-channels':\s*\(\)\s*=> batchEnableSelectedChannels\(\)/);
  assert.match(initScript, /'batch-delete-channels':\s*\(\)\s*=> batchDeleteSelectedChannels\(\)/);
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
