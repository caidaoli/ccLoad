async function testChannel(id, name) {
  const channel = channels.find(c => c.id === id);
  if (!channel) return;

  testingChannelId = id;
  document.getElementById('testChannelName').textContent = name;

  const modelSelect = document.getElementById('testModelSelect');
  modelSelect.innerHTML = '';
  channel.models.forEach(model => {
    const option = document.createElement('option');
    option.value = model;
    option.textContent = model;
    modelSelect.appendChild(option);
  });

  let apiKeys = [];
  try {
    const res = await fetchWithAuth(`/admin/channels/${id}/keys`);
    if (res.ok) {
      const data = await res.json();
      apiKeys = (data.success ? data.data : data) || [];
    }
  } catch (e) {
    console.error('获取API Keys失败', e);
  }

  const keys = apiKeys.map(k => k.api_key || k);
  const keySelect = document.getElementById('testKeySelect');
  const keySelectGroup = document.getElementById('testKeySelectGroup');
  const batchTestBtn = document.getElementById('batchTestBtn');

  if (keys.length > 1) {
    keySelectGroup.style.display = 'block';
    batchTestBtn.style.display = 'inline-block';
    
    keySelect.innerHTML = '';
    const maxKeys = Math.min(keys.length, 10);
    for (let i = 0; i < maxKeys; i++) {
      const option = document.createElement('option');
      option.value = i;
      option.textContent = `Key ${i + 1}: ${maskKey(keys[i])}`;
      keySelect.appendChild(option);
    }
    
    if (keys.length > 10) {
      const hintOption = document.createElement('option');
      hintOption.disabled = true;
      hintOption.textContent = `... 还有 ${keys.length - 10} 个Key（使用批量测试）`;
      keySelect.appendChild(hintOption);
    }
  } else {
    keySelectGroup.style.display = 'none';
    batchTestBtn.style.display = 'none';
  }

  resetTestModal();

  const channelType = channel.channel_type || 'anthropic';
  await window.ChannelTypeManager.renderChannelTypeSelect('testChannelType', channelType);

  document.getElementById('testModal').classList.add('show');
}

function closeTestModal() {
  document.getElementById('testModal').classList.remove('show');
  testingChannelId = null;
}

function resetTestModal() {
  document.getElementById('testProgress').classList.remove('show');
  document.getElementById('batchTestProgress').style.display = 'none';
  document.getElementById('testResult').classList.remove('show', 'success', 'error');
  document.getElementById('runTestBtn').disabled = false;
  document.getElementById('batchTestBtn').disabled = false;
  document.getElementById('testContentInput').value = defaultTestContent;
  document.getElementById('testChannelType').value = 'anthropic';
  document.getElementById('testConcurrency').value = '10';
}

function updateTestURL() {}

async function runChannelTest() {
  if (!testingChannelId) return;

  const modelSelect = document.getElementById('testModelSelect');
  const contentInput = document.getElementById('testContentInput');
  const channelTypeSelect = document.getElementById('testChannelType');
  const keySelect = document.getElementById('testKeySelect');
  const streamCheckbox = document.getElementById('testStreamEnabled');
  const selectedModel = modelSelect.value;
  const testContent = contentInput.value.trim() || defaultTestContent;
  const channelType = channelTypeSelect.value;
  const streamEnabled = streamCheckbox.checked;

  if (!selectedModel) {
    if (window.showError) showError('请选择一个模型');
    return;
  }

  document.getElementById('testProgress').classList.add('show');
  document.getElementById('testResult').classList.remove('show');
  document.getElementById('runTestBtn').disabled = true;

  try {
    const testRequest = {
      model: selectedModel,
      max_tokens: 512,
      stream: streamEnabled,
      content: testContent,
      channel_type: channelType
    };

    if (keySelect && keySelect.parentElement.style.display !== 'none') {
      testRequest.key_index = parseInt(keySelect.value) || 0;
    }

    const res = await fetchWithAuth(`/admin/channels/${testingChannelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(testRequest)
    });

    if (!res.ok) {
      throw new Error('HTTP ' + res.status);
    }

    const result = await res.json();
    const testResult = result.data || result;
    displayTestResult(testResult);
  } catch (e) {
    console.error('测试失败', e);

    displayTestResult({
      success: false,
      error: '测试请求失败: ' + e.message
    });
  } finally {
    document.getElementById('testProgress').classList.remove('show');
    document.getElementById('runTestBtn').disabled = false;

    clearChannelsCache();
    await loadChannels(filters.channelType);
  }
}

async function runBatchTest() {
  if (!testingChannelId) return;

  const channel = channels.find(c => c.id === testingChannelId);
  if (!channel) return;

  let apiKeys = [];
  try {
    const res = await fetchWithAuth(`/admin/channels/${testingChannelId}/keys`);
    if (res.ok) {
      const data = await res.json();
      apiKeys = (data.success ? data.data : data) || [];
    }
  } catch (e) {
    console.error('获取API Keys失败', e);
  }

  const keys = apiKeys.map(k => k.api_key || k);
  if (keys.length === 0) {
    if (window.showError) showError('没有可用的API Key');
    return;
  }

  const modelSelect = document.getElementById('testModelSelect');
  const contentInput = document.getElementById('testContentInput');
  const channelTypeSelect = document.getElementById('testChannelType');
  const streamCheckbox = document.getElementById('testStreamEnabled');
  const concurrencyInput = document.getElementById('testConcurrency');

  const selectedModel = modelSelect.value;
  const testContent = contentInput.value.trim() || defaultTestContent;
  const channelType = channelTypeSelect.value;
  const streamEnabled = streamCheckbox.checked;
  const concurrency = Math.max(1, Math.min(50, parseInt(concurrencyInput.value) || 10));

  if (!selectedModel) {
    if (window.showError) showError('请选择一个模型');
    return;
  }

  document.getElementById('runTestBtn').disabled = true;
  document.getElementById('batchTestBtn').disabled = true;

  const progressDiv = document.getElementById('batchTestProgress');
  const counterSpan = document.getElementById('batchTestCounter');
  const progressBar = document.getElementById('batchTestProgressBar');
  const statusDiv = document.getElementById('batchTestStatus');
  
  progressDiv.style.display = 'block';
  document.getElementById('testResult').classList.remove('show');

  let successCount = 0;
  let failedCount = 0;
  const failedKeys = [];
  let completedCount = 0;

  const updateProgress = () => {
    const progress = (completedCount / keys.length * 100).toFixed(0);
    counterSpan.textContent = `${completedCount} / ${keys.length}`;
    progressBar.style.width = `${progress}%`;
    statusDiv.textContent = `已完成 ${completedCount} / ${keys.length}（并发数: ${concurrency}）`;
  };

  const testSingleKey = async (keyIndex) => {
    try {
      const testRequest = {
        model: selectedModel,
        max_tokens: 512,
        stream: streamEnabled,
        content: testContent,
        channel_type: channelType,
        key_index: keyIndex
      };

      const res = await fetchWithAuth(`/admin/channels/${testingChannelId}/test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(testRequest)
      });

      const result = await res.json();
      const testResult = result.data || result;

      if (testResult.success) {
        successCount++;
      } else {
        failedCount++;
        failedKeys.push({ index: keyIndex, key: maskKey(keys[keyIndex]), error: testResult.error });
      }
    } catch (e) {
      failedCount++;
      failedKeys.push({ index: keyIndex, key: maskKey(keys[keyIndex]), error: e.message });
    } finally {
      completedCount++;
      updateProgress();
    }
  };

  const batches = [];
  for (let i = 0; i < keys.length; i += concurrency) {
    const batchIndexes = [];
    for (let j = i; j < Math.min(i + concurrency, keys.length); j++) {
      batchIndexes.push(j);
    }
    batches.push(batchIndexes);
  }

  updateProgress();

  for (const batch of batches) {
    const batchPromises = batch.map(keyIndex => testSingleKey(keyIndex));
    await Promise.all(batchPromises);
  }

  displayBatchTestResult(successCount, failedCount, keys.length, failedKeys);

  document.getElementById('runTestBtn').disabled = false;
  document.getElementById('batchTestBtn').disabled = false;
  
  clearChannelsCache();
  await loadChannels(filters.channelType);
}

function displayBatchTestResult(successCount, failedCount, totalCount, failedKeys) {
  const testResultDiv = document.getElementById('testResult');
  const contentDiv = document.getElementById('testResultContent');
  const detailsDiv = document.getElementById('testResultDetails');
  const statusDiv = document.getElementById('batchTestStatus');

  testResultDiv.classList.remove('success', 'error');
  testResultDiv.classList.add('show');

  statusDiv.textContent = `完成！成功: ${successCount}, 失败: ${failedCount}`;

  if (failedCount === 0) {
    testResultDiv.classList.add('success');
    contentDiv.innerHTML = `
      <div style="display: flex; align-items: center; gap: 8px;">
        <span style="font-size: 18px;">✅</span>
        <strong>批量测试完成：全部 ${totalCount} 个Key测试成功</strong>
      </div>
    `;
    detailsDiv.innerHTML = '';
  } else if (successCount === 0) {
    testResultDiv.classList.add('error');
    contentDiv.innerHTML = `
      <div style="display: flex; align-items: center; gap: 8px;">
        <span style="font-size: 18px;">❌</span>
        <strong>批量测试完成：全部 ${totalCount} 个Key测试失败</strong>
      </div>
    `;
    
    let details = '<h4 style="margin-top: 12px; color: var(--error-600);">失败详情：</h4><ul style="margin: 8px 0; padding-left: 20px;">';
    failedKeys.forEach(({ index, key, error }) => {
      details += `<li style="margin: 4px 0;"><strong>Key #${index + 1}</strong> (${key}): ${escapeHtml(error)}</li>`;
    });
    details += '</ul><p style="color: var(--error-600); margin-top: 8px;">失败的Key已自动冷却</p>';
    detailsDiv.innerHTML = details;
  } else {
    testResultDiv.classList.add('success');
    contentDiv.innerHTML = `
      <div style="display: flex; align-items: center; gap: 8px;">
        <span style="font-size: 18px;">⚠️</span>
        <strong>批量测试完成：${successCount} 个成功，${failedCount} 个失败</strong>
      </div>
    `;
    
    let details = `<p style="color: var(--success-600);">✅ ${successCount} 个Key可用</p>`;
    details += '<h4 style="margin-top: 12px; color: var(--error-600);">失败详情：</h4><ul style="margin: 8px 0; padding-left: 20px;">';
    failedKeys.forEach(({ index, key, error }) => {
      details += `<li style="margin: 4px 0;"><strong>Key #${index + 1}</strong> (${key}): ${escapeHtml(error)}</li>`;
    });
    details += '</ul><p style="color: var(--error-600); margin-top: 8px;">失败的Key已自动冷却</p>';
    detailsDiv.innerHTML = details;
  }
}

function displayTestResult(result) {
  const testResultDiv = document.getElementById('testResult');
  const contentDiv = document.getElementById('testResultContent');
  const detailsDiv = document.getElementById('testResultDetails');

  testResultDiv.classList.remove('success', 'error');
  testResultDiv.classList.add('show');

  if (result.success) {
    testResultDiv.classList.add('success');
    contentDiv.innerHTML = `
      <div style="display: flex; align-items: center; gap: 8px;">
        <span style="font-size: 18px;">✅</span>
        <strong>${result.message || 'API测试成功'}</strong>
      </div>
    `;
    
    let details = `响应时间: ${result.duration_ms}ms`;
    if (result.status_code) {
      details += ` | 状态码: ${result.status_code}`;
    }
    
    if (result.response_text) {
      details += `
        <div class="response-section">
          <h4>API 响应内容</h4>
          <div class="response-content">${escapeHtml(result.response_text)}</div>
        </div>
      `;
    }
    
    if (result.api_response) {
      const responseId = 'api-response-' + Date.now();
      details += `
        <div class="response-section">
          <h4>完整 API 响应</h4>
          <button class="toggle-btn" onclick="toggleResponse('${responseId}')">显示/隐藏 JSON</button>
          <div id="${responseId}" class="response-content" style="display: none;">${escapeHtml(JSON.stringify(result.api_response, null, 2))}</div>
        </div>
      `;
    } else if (result.raw_response) {
      const rawId = 'raw-response-' + Date.now();
      details += `
        <div class="response-section">
          <h4>原始响应</h4>
          <button class="toggle-btn" onclick="toggleResponse('${rawId}')">显示/隐藏</button>
          <div id="${rawId}" class="response-content" style="display: none;">${escapeHtml(result.raw_response)}</div>
        </div>
      `;
    }
    
    detailsDiv.innerHTML = details;
  } else {
    testResultDiv.classList.add('error');
    contentDiv.innerHTML = `
      <div style="display: flex; align-items: center; gap: 8px;">
        <span style="font-size: 18px;">❌</span>
        <strong>测试失败</strong>
      </div>
    `;
    
    let details = result.error || '未知错误';
    if (result.duration_ms) {
      details += `<br>响应时间: ${result.duration_ms}ms`;
    }
    if (result.status_code) {
      details += ` | 状态码: ${result.status_code}`;
    }
    
    if (result.api_error) {
      const errorId = 'api-error-' + Date.now();
      details += `
        <div class="response-section">
          <h4>完整错误响应</h4>
          <button class="toggle-btn" onclick="toggleResponse('${errorId}')">显示/隐藏 JSON</button>
          <div id="${errorId}" class="response-content" style="display: block;">${escapeHtml(JSON.stringify(result.api_error, null, 2))}</div>
        </div>
      `;
    }
    if (typeof result.raw_response !== 'undefined') {
      const rawId = 'raw-error-' + Date.now();
      details += `
        <div class="response-section">
          <h4>原始错误响应</h4>
          <button class="toggle-btn" onclick="toggleResponse('${rawId}')">显示/隐藏</button>
          <div id="${rawId}" class="response-content" style="display: block;">${escapeHtml(result.raw_response || '(无响应体)')}</div>
        </div>
      `;
    }
    if (result.response_headers) {
      const headersId = 'resp-headers-' + Date.now();
      details += `
        <div class="response-section">
          <h4>响应头</h4>
          <button class="toggle-btn" onclick="toggleResponse('${headersId}')">显示/隐藏</button>
          <div id="${headersId}" class="response-content" style="display: block;">${escapeHtml(JSON.stringify(result.response_headers, null, 2))}</div>
        </div>
      `;
    }
    
    detailsDiv.innerHTML = details;
  }
}
