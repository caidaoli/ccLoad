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
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${id}/keys`)) || [];
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
    if (window.showError) window.showError('请选择一个模型');
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

    const testResult = await fetchDataWithAuth(`/admin/channels/${testingChannelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(testRequest)
    });
    displayTestResult(testResult || { success: false, error: '空响应' });
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
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${testingChannelId}/keys`)) || [];
  } catch (e) {
    console.error('获取API Keys失败', e);
  }

  const keys = apiKeys.map(k => k.api_key || k);
  if (keys.length === 0) {
    if (window.showError) window.showError('没有可用的API Key');
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
    if (window.showError) window.showError('请选择一个模型');
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

      const testResult = await fetchDataWithAuth(`/admin/channels/${testingChannelId}/test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(testRequest)
      });

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

  // 使用模板渲染头部
  const renderHeader = (icon, message) => {
    const header = TemplateEngine.render('tpl-test-result-header', { icon, message });
    contentDiv.innerHTML = '';
    if (header) contentDiv.appendChild(header);
  };

  // 构建失败详情列表
  const buildFailDetails = () => {
    const items = failedKeys.map(({ index, key, error }) => {
      const item = TemplateEngine.render('tpl-batch-fail-item', {
        keyNum: index + 1,
        keyMask: key,
        error: escapeHtml(error)
      });
      return item ? item.outerHTML : '';
    }).join('');
    return `<ul style="margin: 8px 0; padding-left: 20px;">${items}</ul>`;
  };

  if (failedCount === 0) {
    testResultDiv.classList.add('success');
    renderHeader('✅', `批量测试完成：全部 ${totalCount} 个Key测试成功`);
    detailsDiv.innerHTML = '';
  } else if (successCount === 0) {
    testResultDiv.classList.add('error');
    renderHeader('❌', `批量测试完成：全部 ${totalCount} 个Key测试失败`);
    detailsDiv.innerHTML = `<h4 style="margin-top: 12px; color: var(--error-600);">失败详情：</h4>${buildFailDetails()}<p style="color: var(--error-600); margin-top: 8px;">失败的Key已自动冷却</p>`;
  } else {
    testResultDiv.classList.add('success');
    renderHeader('⚠️', `批量测试完成：${successCount} 个成功，${failedCount} 个失败`);
    detailsDiv.innerHTML = `<p style="color: var(--success-600);">✅ ${successCount} 个Key可用</p><h4 style="margin-top: 12px; color: var(--error-600);">失败详情：</h4>${buildFailDetails()}<p style="color: var(--error-600); margin-top: 8px;">失败的Key已自动冷却</p>`;
  }
}

function displayTestResult(result) {
  const testResultDiv = document.getElementById('testResult');
  const contentDiv = document.getElementById('testResultContent');
  const detailsDiv = document.getElementById('testResultDetails');

  testResultDiv.classList.remove('success', 'error');
  testResultDiv.classList.add('show');

  // 使用模板渲染头部
  const renderHeader = (icon, message) => {
    const header = TemplateEngine.render('tpl-test-result-header', { icon, message });
    contentDiv.innerHTML = '';
    if (header) contentDiv.appendChild(header);
  };

  // 渲染响应区块
  const renderResponseSection = (title, content, display = 'none', hasToggle = true) => {
    const contentId = `response-${Date.now()}-${Math.random().toString(36).substr(2, 9)}`;
    const toggleBtn = hasToggle ? `<button class="toggle-btn" onclick="toggleResponse('${contentId}')">显示/隐藏</button>` : '';
    const section = TemplateEngine.render('tpl-response-section', {
      title,
      toggleBtn,
      contentId,
      display,
      content: escapeHtml(content)
    });
    return section ? section.outerHTML : '';
  };

  if (result.success) {
    testResultDiv.classList.add('success');
    renderHeader('✅', result.message || 'API测试成功');

    let details = `响应时间: ${result.duration_ms}ms`;
    if (result.status_code) {
      details += ` | 状态码: ${result.status_code}`;
    }

    if (result.response_text) {
      details += renderResponseSection('API 响应内容', result.response_text, 'block', false);
    }

    if (result.api_response) {
      details += renderResponseSection('完整 API 响应', JSON.stringify(result.api_response, null, 2));
    } else if (result.raw_response) {
      details += renderResponseSection('原始响应', result.raw_response);
    }

    detailsDiv.innerHTML = details;
  } else {
    testResultDiv.classList.add('error');
    renderHeader('❌', '测试失败');

    // [FIX] 转义 result.error 防止 XSS
    let details = escapeHtml(result.error || '未知错误');
    if (result.duration_ms) {
      details += `<br>响应时间: ${result.duration_ms}ms`;
    }
    if (result.status_code) {
      details += ` | 状态码: ${result.status_code}`;
    }

    if (result.api_error) {
      details += renderResponseSection('完整错误响应', JSON.stringify(result.api_error, null, 2), 'block');
    }
    if (typeof result.raw_response !== 'undefined') {
      details += renderResponseSection('原始错误响应', result.raw_response || '(无响应体)', 'block');
    }
    if (result.response_headers) {
      details += renderResponseSection('响应头', JSON.stringify(result.response_headers, null, 2), 'block');
    }

    detailsDiv.innerHTML = details;
  }
}
