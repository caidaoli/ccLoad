/**
 * model-fingerprint.js — 指纹对比模式 UI
 *
 * 依赖（在 model-test.html 中于本脚本之前加载）：
 *   - ui.js          → fetchDataWithAuth / createSearchableCombobox / i18nText
 *   - model-test.js  → channelsList (全局)
 *
 * 暴露：window.ModelFingerprint.init()
 * model-test.js 在 switchTestMode('fingerprint') 时调用 init()。
 */
(function () {
  'use strict';

  // ─── 常量 ───────────────────────────────────────────────────────────────
  const DEFAULT_ITERATIONS  = 100;
  const DEFAULT_CONCURRENCY = 5;

  // ─── 状态 ───────────────────────────────────────────────────────────────
  let fingerprints  = [];   // GET /admin/fingerprints 返回列表
  let testHistory   = [];   // GET /admin/fingerprints/test-results 返回列表
  let activeJobId   = null;
  let streamAbort   = null; // AbortController for SSE
  let initialized   = false;

  // combobox 实例
  let calChannelCombo = null;
  let calModelCombo   = null;
  let tstChannelCombo = null;
  let tstModelCombo   = null;

  // ─── DOM 引用（延迟获取）────────────────────────────────────────────────
  function el(id) { return document.getElementById(id); }

  // ─── i18n 辅助 ──────────────────────────────────────────────────────────
  function t(key, fallback) {
    return (typeof window.i18nText === 'function')
      ? window.i18nText(key, fallback || key)
      : (fallback || key);
  }

  // ─── API 调用 ────────────────────────────────────────────────────────────
  async function apiData(url, options) {
    return window.fetchDataWithAuth(url, options);
  }

  // ─── 渠道/模型数据 ─────────────────────────────────────────────────────
  async function ensureChannels() {
    let channels = window.channelsList;
    if (Array.isArray(channels) && channels.length) return channels;
    if (typeof window.fetchDataWithAuth === 'function') {
      try {
        channels = (await window.fetchDataWithAuth('/admin/channels')) || [];
        window.channelsList = channels;
      } catch (_) {
        channels = [];
      }
    }
    return Array.isArray(channels) ? channels : [];
  }

  function channelLabel(ch) {
    return ch.name + ' (#' + ch.id + ')';
  }

  function getChannelOptions() {
    const channels = window.channelsList || [];
    return channels.map(ch => ({ value: String(ch.id), label: channelLabel(ch) }));
  }

  /** 收集所有渠道中的去重模型名。 */
  function getAllModelOptions() {
    const channels = window.channelsList || [];
    const seen = new Set();
    const options = [];
    channels.forEach(ch => {
      const models = ch.models || [];
      models.forEach(m => {
        const name = (typeof m === 'string') ? m : (m.model || m.name || '');
        if (name && !seen.has(name)) {
          seen.add(name);
          options.push({ value: name, label: name });
        }
      });
    });
    return options;
  }

  /** 过滤出包含指定模型的渠道。 */
  function getChannelOptionsForModel(modelName) {
    if (!modelName) return getChannelOptions();
    const channels = window.channelsList || [];
    return channels
      .filter(ch => {
        const models = ch.models || [];
        return models.some(m => {
          const name = (typeof m === 'string') ? m : (m.model || m.name || '');
          return name === modelName;
        });
      })
      .map(ch => ({ value: String(ch.id), label: channelLabel(ch) }));
  }

  /** 获取指定渠道支持的模型列表。 */
  function getModelOptions(channelId) {
    const channels = window.channelsList || [];
    const ch = channels.find(c => String(c.id) === String(channelId));
    const models = (ch && ch.models) ? ch.models : [];
    return models
      .map(m => (typeof m === 'string') ? m : (m.model || m.name || ''))
      .filter(Boolean)
      .map(name => ({ value: name, label: name }));
  }

  // ─── combobox 创建 ─────────────────────────────────────────────────────

  /** 创建模型 combobox（全量模型）。 */
  function createAllModelCombo(containerId, hiddenId, onModelChange) {
    if (typeof window.createSearchableCombobox !== 'function') return null;
    return window.createSearchableCombobox({
      container: containerId,
      inputId: hiddenId + '_input',
      dropdownId: hiddenId + '_dropdown',
      placeholder: t('modelTest.fingerprint.selectModel', '搜索模型...'),
      minWidth: 120,
      getOptions: getAllModelOptions,
      onSelect: (value) => {
        const hidden = el(hiddenId);
        if (hidden) hidden.value = value;
        if (onModelChange) onModelChange(value);
      }
    });
  }

  /** 创建渠道 combobox（可按模型过滤）。 */
  function createFilteredChannelCombo(containerId, hiddenId, modelName) {
    if (typeof window.createSearchableCombobox !== 'function') return null;
    return window.createSearchableCombobox({
      container: containerId,
      inputId: hiddenId + '_input',
      dropdownId: hiddenId + '_dropdown',
      placeholder: t('modelTest.fingerprint.selectChannel', '搜索渠道...'),
      minWidth: 120,
      getOptions: () => getChannelOptionsForModel(modelName),
      onSelect: (value) => {
        const hidden = el(hiddenId);
        if (hidden) hidden.value = value;
      }
    });
  }

  /** 标定：渠道 combobox（选渠道后联动模型）。 */
  function createCalChannelCombo() {
    if (typeof window.createSearchableCombobox !== 'function') return null;
    return window.createSearchableCombobox({
      container: 'fpCalibrateChannelContainer',
      inputId: 'fpCalibrateChannel_input',
      dropdownId: 'fpCalibrateChannel_dropdown',
      placeholder: t('modelTest.fingerprint.selectChannel', '搜索渠道...'),
      minWidth: 120,
      getOptions: () => getChannelOptionsForModel(el('fpCalibrateModel')?.value || ''),
      onSelect: (value) => {
        const hidden = el('fpCalibrateChannel');
        if (hidden) hidden.value = value;
      }
    });
  }

  /** 标定：模型 combobox（选模型后联动重建渠道 combobox）。 */
  function createCalModelCombo() {
    if (typeof window.createSearchableCombobox !== 'function') return null;
    return window.createSearchableCombobox({
      container: 'fpCalibrateModelContainer',
      inputId: 'fpCalibrateModel_input',
      dropdownId: 'fpCalibrateModel_dropdown',
      placeholder: t('modelTest.fingerprint.selectModel', '搜索模型...'),
      minWidth: 120,
      getOptions: getAllModelOptions,
      onSelect: (value) => {
        const hidden = el('fpCalibrateModel');
        if (hidden) hidden.value = value;
        onCalModelChange(value);
      }
    });
  }

  // ─── 标定：模型→渠道联动 ──────────────────────────────────────────────
  function onCalModelChange(modelName) {
    if (calChannelCombo) calChannelCombo.destroy();
    const hidden = el('fpCalibrateChannel');
    if (hidden) hidden.value = '';
    calChannelCombo = createFilteredChannelCombo('fpCalibrateChannelContainer', 'fpCalibrateChannel', modelName);
  }

  // ─── 对比：模型→渠道联动 + 基准自动选择 ────────────────────────────────
  function onTstModelChange(modelName) {
    // 重建渠道 combobox（按模型过滤）
    if (tstChannelCombo) tstChannelCombo.destroy();
    const hidden = el('fpTestChannel');
    if (hidden) hidden.value = '';
    tstChannelCombo = createFilteredChannelCombo('fpTestChannelContainer', 'fpTestChannel', modelName);

    // 基准自动选择
    autoSelectBaseline(modelName);
  }

  function autoSelectBaseline(modelName) {
    const select = el('fpTestBaselineSelect');
    if (!select) return;

    // 重建 select 选项
    select.innerHTML = '<option value="">' + t('modelTest.fingerprint.baselineAny', '任意（全量对比）') + '</option>';

    if (!modelName) {
      // 无模型选择时，显示全部基准
      fingerprints.forEach(fp => {
        const opt = document.createElement('option');
        opt.value = fp.id;
        opt.textContent = fp.name + ' (' + (fp.model || '') + ')';
        select.appendChild(opt);
      });
      return;
    }

    const matched = fingerprints.filter(fp => fp.model === modelName);
    if (matched.length === 0) {
      // 无匹配，显示全部基准
      fingerprints.forEach(fp => {
        const opt = document.createElement('option');
        opt.value = fp.id;
        opt.textContent = fp.name + ' (' + (fp.model || '') + ')';
        select.appendChild(opt);
      });
    } else {
      // 只显示匹配模型的基准
      matched.forEach(fp => {
        const opt = document.createElement('option');
        opt.value = fp.id;
        opt.textContent = fp.name + ' (' + (fp.model || '') + ')';
        select.appendChild(opt);
      });
      // 恰好一个匹配 → 自动选中
      if (matched.length === 1) {
        select.value = String(matched[0].id);
      }
    }
  }

  // ─── 基准列表渲染 ──────────────────────────────────────────────────────
  function renderBaselineTable() {
    const tbody = el('fpBaselineTbody');
    const select = el('fpTestBaselineSelect');
    if (!tbody) return;

    tbody.innerHTML = '';
    if (select) {
      select.innerHTML = '<option value="">' + t('modelTest.fingerprint.baselineAny', '任意（全量对比）') + '</option>';
    }

    if (!fingerprints.length) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td colspan="5" style="text-align:center;color:var(--text-muted)">'
        + t('modelTest.fingerprint.noBaselines', '暂无基准，请先标定') + '</td>';
      tbody.appendChild(tr);
      return;
    }

    fingerprints.forEach(fp => {
      const tr = document.createElement('tr');
      const createdAt = fp.created_at ? new Date(fp.created_at * 1000).toLocaleString() : '-';
      tr.innerHTML =
        '<td>' + escHtml(fp.name || '-') + '</td>' +
        '<td>' + escHtml(fp.channel_name || ('#' + fp.channel_id)) + '</td>' +
        '<td>' + escHtml(fp.model || '-') + '</td>' +
        '<td>' + createdAt + '</td>' +
        '<td><button class="btn btn-secondary btn-sm fp-delete-btn" data-id="' + fp.id + '">'
        + t('common.delete', '删除') + '</button></td>';
      tbody.appendChild(tr);

      if (select) {
        const opt = document.createElement('option');
        opt.value = fp.id;
        opt.textContent = fp.name + ' (' + (fp.model || '') + ')';
        select.appendChild(opt);
      }
    });

    tbody.querySelectorAll('.fp-delete-btn').forEach(btn => {
      btn.addEventListener('click', () => deleteFingerprint(btn.dataset.id));
    });

    // 如果对比模型已选，重新执行基准自动选择
    const tstModel = el('fpTestModel')?.value || '';
    if (tstModel) autoSelectBaseline(tstModel);
  }

  // ─── 加载基准列表 ────────────────────────────────────────────────────────
  async function loadFingerprints() {
    try {
      const list = await apiData('/admin/fingerprints');
      fingerprints = Array.isArray(list) ? list : [];
    } catch (e) {
      fingerprints = [];
    }
    renderBaselineTable();
  }

  // ─── 删除基准 ────────────────────────────────────────────────────────────
  async function deleteFingerprint(id) {
    if (!confirm(t('modelTest.fingerprint.confirmDelete', '确认删除此基准？'))) return;
    try {
      await apiData('/admin/fingerprints/' + id, { method: 'DELETE' });
      await loadFingerprints();
    } catch (e) {
      alert(t('modelTest.fingerprint.deleteFailed', '删除失败: ') + (e.message || e));
    }
  }

  // ─── 对比历史 ────────────────────────────────────────────────────────────
  async function loadTestHistory() {
    try {
      const list = await apiData('/admin/fingerprints/test-results');
      testHistory = Array.isArray(list) ? list : [];
    } catch (e) {
      testHistory = [];
    }
    renderHistoryTable();
  }

  function renderHistoryTable() {
    const tbody = el('fpHistoryTbody');
    if (!tbody) return;

    tbody.innerHTML = '';

    if (!testHistory.length) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td colspan="6" style="text-align:center;color:var(--text-muted)">'
        + t('modelTest.fingerprint.noHistory', '暂无对比历史') + '</td>';
      tbody.appendChild(tr);
      return;
    }

    testHistory.forEach(rec => {
      const tr = document.createElement('tr');
      tr.className = 'fp-history-row';
      const createdAt = rec.created_at ? new Date(rec.created_at * 1000).toLocaleString() : '-';
      const scoreNum = typeof rec.best_score === 'number' ? rec.best_score : null;
      const score = scoreNum != null ? (scoreNum * 100).toFixed(1) + '%' : '-';
      const hint = scoreHint(scoreNum);
      tr.innerHTML =
        '<td>' + escHtml(rec.model || '-') + '</td>' +
        '<td>' + escHtml(rec.channel_name || (rec.channel_id ? '#' + rec.channel_id : '-')) + '</td>' +
        '<td>' + (rec.sample_count || '-') + '</td>' +
        '<td class="fp-score">' + score + (hint ? ' <span class="fp-score-hint">(' + escHtml(hint) + ')</span>' : '') + '</td>' +
        '<td>' + createdAt + '</td>' +
        '<td>' +
          '<button class="btn btn-secondary btn-sm fp-history-detail-btn" data-id="' + rec.id + '">' + t('common.detail', '详情') + '</button> ' +
          '<button class="btn btn-secondary btn-sm fp-history-delete-btn" data-id="' + rec.id + '">' + t('common.delete', '删除') + '</button>' +
        '</td>';
      tbody.appendChild(tr);

      // 详情展开行（隐藏）
      const detailTr = document.createElement('tr');
      detailTr.className = 'fp-history-detail hidden';
      detailTr.id = 'fpHistoryDetail_' + rec.id;
      detailTr.innerHTML = '<td colspan="6"><div class="fp-history-detail-content"></div></td>';
      tbody.appendChild(detailTr);
    });

    tbody.querySelectorAll('.fp-history-detail-btn').forEach(btn => {
      btn.addEventListener('click', () => toggleHistoryDetail(btn.dataset.id));
    });

    tbody.querySelectorAll('.fp-history-delete-btn').forEach(btn => {
      btn.addEventListener('click', () => deleteTestResult(btn.dataset.id));
    });
  }

  function toggleHistoryDetail(id) {
    const detailTr = el('fpHistoryDetail_' + id);
    if (!detailTr) return;

    if (!detailTr.classList.contains('hidden')) {
      detailTr.classList.add('hidden');
      return;
    }

    // 展开：渲染 matches
    const rec = testHistory.find(r => String(r.id) === String(id));
    if (!rec) return;

    let matches = rec.matches;
    if (!matches && rec.matches_json) {
      try { matches = JSON.parse(rec.matches_json); } catch (_) { matches = []; }
    }

    const content = detailTr.querySelector('.fp-history-detail-content');
    if (!content) return;

    if (!Array.isArray(matches) || !matches.length) {
      content.innerHTML = '<span style="color:var(--text-muted)">' + t('modelTest.fingerprint.noResult', '无结果') + '</span>';
    } else {
      const table = document.createElement('table');
      table.className = 'modern-table fp-result-table';
      table.innerHTML =
        '<thead><tr>' +
        '<th>' + t('modelTest.fingerprint.col.baseline', '基准') + '</th>' +
        '<th>' + t('modelTest.fingerprint.col.score', '综合评分') + '</th>' +
        '<th>' + t('modelTest.fingerprint.col.cosine', '余弦相似') + '</th>' +
        '<th>' + t('modelTest.fingerprint.col.js', 'JS散度') + '</th>' +
        '<th>' + t('modelTest.fingerprint.col.modeMatch', '众数匹配') + '</th>' +
        '</tr></thead>';

      const mtbody = document.createElement('tbody');
      matches.forEach(m => {
        const mtr = document.createElement('tr');
        const s = typeof m.score === 'number' ? (m.score * 100).toFixed(1) + '%' : '-';
        const cosine = typeof m.cosine_similarity === 'number' ? m.cosine_similarity.toFixed(4) : '-';
        const js = typeof m.js_divergence === 'number' ? m.js_divergence.toFixed(4) : '-';
        const modeMatch = m.mode_match ? '✓' : '✗';
        const baselineName = (m.baseline && m.baseline.name) ? escHtml(m.baseline.name) : '-';
        const mhint = scoreHint(typeof m.score === 'number' ? m.score : null);
        mtr.innerHTML =
          '<td>' + baselineName + '</td>' +
          '<td class="fp-score">' + s + (mhint ? ' <span class="fp-score-hint">(' + escHtml(mhint) + ')</span>' : '') + '</td>' +
          '<td>' + cosine + '</td>' +
          '<td>' + js + '</td>' +
          '<td>' + modeMatch + '</td>';
        mtbody.appendChild(mtr);
      });
      table.appendChild(mtbody);
      content.innerHTML = '';
      content.appendChild(table);
    }

    detailTr.classList.remove('hidden');
  }

  async function deleteTestResult(id) {
    if (!confirm(t('modelTest.fingerprint.confirmDeleteHistory', '确认删除此对比记录？'))) return;
    try {
      await apiData('/admin/fingerprints/test-results/' + id, { method: 'DELETE' });
      await loadTestHistory();
    } catch (e) {
      alert(t('modelTest.fingerprint.deleteFailed', '删除失败: ') + (e.message || e));
    }
  }

  // ─── 进度 UI ────────────────────────────────────────────────────────────
  function showProgress(text, opts) {
    const p = el('fpProgress');
    const fill = el('fpProgressFill');
    const textEl = el('fpProgressText');
    if (!p) return;

    const pct = (opts && typeof opts.pct === 'number')
      ? Math.max(0, Math.min(100, opts.pct))
      : null;
    const state = (opts && opts.state) || '';

    p.classList.remove('hidden');
    if (textEl) textEl.textContent = text || '';

    if (fill) {
      if (pct != null) fill.style.width = pct + '%';
      fill.classList.remove('is-done', 'is-failed', 'is-cancelled');
      if (state === 'succeeded') fill.classList.add('is-done');
      else if (state === 'failed') fill.classList.add('is-failed');
      else if (state === 'cancelled') fill.classList.add('is-cancelled');
    }
  }

  function hideProgress() {
    const p = el('fpProgress');
    const fill = el('fpProgressFill');
    const textEl = el('fpProgressText');
    if (p) p.classList.add('hidden');
    if (fill) {
      fill.style.width = '0%';
      fill.classList.remove('is-done', 'is-failed', 'is-cancelled');
    }
    if (textEl) textEl.textContent = '';
  }

  function setRunning(running) {
    const calibrateBtn       = el('fpCalibrateBtn');
    const calibrateCancelBtn = el('fpCalibrateCancelBtn');
    const testBtn            = el('fpTestBtn');
    const cancelBtn          = el('fpCancelBtn');
    if (calibrateBtn)       calibrateBtn.disabled = running;
    if (calibrateCancelBtn) calibrateCancelBtn.classList.toggle('hidden', !running);
    if (testBtn)            testBtn.disabled = running;
    if (cancelBtn)          cancelBtn.classList.toggle('hidden', !running);
  }

  function progressFromJob(job) {
    const p = job && job.progress;
    if (!p || typeof p !== 'object') {
      return { pct: 0, text: t('modelTest.fingerprint.running', '运行中…') };
    }
    const done = Number(p.done) || 0;
    const total = Number(p.total) || 0;
    const success = Number(p.success) || 0;
    const failed = Number(p.failed) || 0;
    const pct = total > 0 ? Math.round((done / total) * 100) : 0;
    const text = t('modelTest.fingerprint.progress', '进度')
      + ': ' + done + '/' + total
      + ' (' + success + ' ok / ' + failed + ' fail)'
      + (job.status ? ' — ' + job.status : '');
    return { pct, text };
  }

  // ─── 结果渲染 ────────────────────────────────────────────────────────────
  function renderCalibrateResult(result) {
    const div = el('fpResults');
    if (!div) return;
    div.innerHTML = '';

    if (!result) {
      div.textContent = t('modelTest.fingerprint.noResult', '无结果');
      return;
    }

    const info = document.createElement('div');
    info.className = 'fp-result-info';
    info.innerHTML =
      '<strong>' + t('modelTest.fingerprint.calibrateSuccess', '标定完成') + '</strong>: ' +
      escHtml(result.name || '') + ' — ' +
      t('modelTest.fingerprint.sampleCount', '样本') + ': ' + (result.sample_count || '-') +
      (result.model ? (' &nbsp;|&nbsp; ' + t('common.model', '模型') + ': ' + escHtml(result.model)) : '');
    div.appendChild(info);
    div.classList.remove('hidden');
  }

  function renderTestResult(result) {
    const div = el('fpResults');
    if (!div) return;
    div.innerHTML = '';

    if (!result || !Array.isArray(result.matches) || !result.matches.length) {
      div.textContent = t('modelTest.fingerprint.noResult', '无结果');
      return;
    }

    const table = document.createElement('table');
    table.className = 'modern-table fp-result-table';
    table.innerHTML =
      '<thead><tr>' +
      '<th>' + t('modelTest.fingerprint.col.baseline', '基准') + '</th>' +
      '<th>' + t('modelTest.fingerprint.col.score', '综合评分') + '</th>' +
      '<th>' + t('modelTest.fingerprint.col.cosine', '余弦相似') + '</th>' +
      '<th>' + t('modelTest.fingerprint.col.js', 'JS散度') + '</th>' +
      '<th>' + t('modelTest.fingerprint.col.modeMatch', '众数匹配') + '</th>' +
      '</tr></thead>';

    const tbody = document.createElement('tbody');
    result.matches.forEach(m => {
      const tr = document.createElement('tr');
      const scoreNum = typeof m.score === 'number' ? m.score : null;
      const score = scoreNum != null ? (scoreNum * 100).toFixed(1) + '%' : '-';
      const cosine = typeof m.cosine_similarity === 'number' ? m.cosine_similarity.toFixed(4) : '-';
      const js = typeof m.js_divergence === 'number' ? m.js_divergence.toFixed(4) : '-';
      const modeMatch = m.mode_match ? '✓' : '✗';
      const baselineName = (m.baseline && m.baseline.name) ? escHtml(m.baseline.name) : '-';
      const hint = scoreHint(scoreNum);
      tr.innerHTML =
        '<td>' + baselineName + '</td>' +
        '<td class="fp-score">' + score + (hint ? ' <span class="fp-score-hint">(' + escHtml(hint) + ')</span>' : '') + '</td>' +
        '<td>' + cosine + '</td>' +
        '<td>' + js + '</td>' +
        '<td>' + modeMatch + '</td>';
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    div.appendChild(table);

    if (result.stats || result.sample_count != null) {
      const summary = document.createElement('div');
      summary.className = 'fp-result-summary';
      summary.textContent = t('modelTest.fingerprint.sampleCount', '样本') + ': '
        + (result.sample_count != null ? result.sample_count : '-');
      div.appendChild(summary);
    }

    div.classList.remove('hidden');
  }

  function scoreHint(score) {
    if (score == null || typeof score !== 'number') return '';
    if (score >= 0.85) return t('modelTest.fingerprint.hint.high', '高度一致');
    if (score >= 0.65) return t('modelTest.fingerprint.hint.medium', '中等一致（建议加大采样复核）');
    return t('modelTest.fingerprint.hint.low', '明显不一致（疑似换模/掺假）');
  }

  // ─── Job SSE 流 ─────────────────────────────────────────────────────────
  // EventSource 不能带 Authorization，与 chat 一样用 fetch + ReadableStream。
  async function startJobStream(jobId, onComplete) {
    stopJobStream();
    activeJobId = jobId;
    showProgress(t('modelTest.fingerprint.running', '运行中…'), { pct: 0 });

    const controller = new AbortController();
    streamAbort = controller;
    const token = localStorage.getItem('ccload_token');

    try {
      const resp = await fetch('/admin/fingerprints/jobs/' + encodeURIComponent(jobId) + '/stream', {
        method: 'GET',
        headers: token ? { 'Authorization': 'Bearer ' + token } : {},
        signal: controller.signal,
      });
      if (!resp.ok || !resp.body) {
        throw new Error('HTTP ' + resp.status);
      }

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      let terminal = false;

      while (!terminal) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });

        let idx;
        while ((idx = buf.indexOf('\n\n')) !== -1) {
          const block = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          for (const line of block.split('\n')) {
            if (!line.startsWith('data:')) continue;
            const payload = line.slice(5).trim();
            if (!payload || payload === '[DONE]') continue;
            let job;
            try { job = JSON.parse(payload); } catch (_) { continue; }
            if (!job) continue;

            const { pct, text } = progressFromJob(job);
            const status = job.status || '';

            if (status === 'succeeded' || status === 'failed' || status === 'cancelled') {
              terminal = true;
              setRunning(false);
              if (status === 'succeeded') {
                showProgress(text, { pct: 100, state: 'succeeded' });
                hideProgress();
                if (typeof onComplete === 'function') onComplete(job.result);
              } else if (status === 'cancelled') {
                showProgress(t('modelTest.fingerprint.cancelled', '已取消'), {
                  pct: pct, state: 'cancelled'
                });
              } else {
                showProgress(
                  t('modelTest.fingerprint.failed', '失败: ') + (job.error || ''),
                  { pct: pct, state: 'failed' }
                );
              }
              break;
            }

            showProgress(text, { pct: pct });
          }
        }
      }

      // 流提前结束且未到终态：回退一次 GET
      if (!terminal && !controller.signal.aborted) {
        try {
          const job = await apiData('/admin/fingerprints/jobs/' + encodeURIComponent(jobId));
          if (job) {
            const status = job.status || '';
            const { pct, text } = progressFromJob(job);
            if (status === 'succeeded') {
              setRunning(false);
              hideProgress();
              if (typeof onComplete === 'function') onComplete(job.result);
            } else if (status === 'cancelled') {
              setRunning(false);
              showProgress(t('modelTest.fingerprint.cancelled', '已取消'), {
                pct: pct, state: 'cancelled'
              });
            } else if (status === 'failed') {
              setRunning(false);
              showProgress(
                t('modelTest.fingerprint.failed', '失败: ') + (job.error || ''),
                { pct: pct, state: 'failed' }
              );
            } else {
              setRunning(false);
              showProgress(text || t('modelTest.fingerprint.failed', '失败: ') + 'stream closed', {
                pct: pct, state: 'failed'
              });
            }
          }
        } catch (_) {
          setRunning(false);
          showProgress(t('modelTest.fingerprint.failed', '失败: ') + 'stream closed', {
            pct: 0, state: 'failed'
          });
        }
      }
    } catch (e) {
      if (e && e.name === 'AbortError') return;
      setRunning(false);
      showProgress(
        t('modelTest.fingerprint.failed', '失败: ') + (e && e.message ? e.message : e),
        { pct: 0, state: 'failed' }
      );
    } finally {
      if (streamAbort === controller) streamAbort = null;
    }
  }

  function stopJobStream() {
    if (streamAbort) {
      try { streamAbort.abort(); } catch (_) { /* ignore */ }
      streamAbort = null;
    }
  }

  // 兼容旧导出名
  function stopPoll() { stopJobStream(); }

  // ─── 取消 Job ────────────────────────────────────────────────────────────
  async function cancelJob() {
    if (!activeJobId) return;
    try {
      await apiData('/admin/fingerprints/jobs/' + activeJobId + '/cancel', { method: 'POST' });
    } catch (_) { /* ignore */ }
  }

  // ─── 标定表单提交 ────────────────────────────────────────────────────────
  async function onCalibrateSubmit() {
    const name        = (el('fpCalibrateName')?.value || '').trim();
    const channelId   = parseInt(el('fpCalibrateChannel')?.value || '0', 10);
    const model       = (el('fpCalibrateModel')?.value || '').trim();
    const iterations  = parseInt(el('fpCalibrateIterations')?.value || DEFAULT_ITERATIONS, 10);
    const concurrency = parseInt(el('fpCalibrateConcurrency')?.value || DEFAULT_CONCURRENCY, 10);

    if (!name)      { alert(t('modelTest.fingerprint.needName', '请输入基准名称')); return; }
    if (!channelId) { alert(t('modelTest.fingerprint.needChannel', '请选择渠道')); return; }
    if (!model)     { alert(t('modelTest.fingerprint.needModel', '请选择模型')); return; }

    const confirmMsg = t('modelTest.fingerprint.costConfirm', '将向渠道发起约 {n} 次请求，产生实际上游费用。是否继续？')
      .replace('{n}', iterations);
    if (!confirm(confirmMsg)) return;

    setRunning(true);
    hideProgress();
    const resultsDiv = el('fpResults');
    if (resultsDiv) resultsDiv.innerHTML = '';

    try {
      const data = await apiData('/admin/fingerprints/calibrate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, channel_id: channelId, model, iterations, concurrency })
      });
      const jobId = data && data.job_id;
      if (!jobId) throw new Error(t('modelTest.fingerprint.startFailed', '启动失败: ') + 'empty job_id');
      startJobStream(jobId, (result) => {
        renderCalibrateResult(result);
        loadFingerprints();
      });
    } catch (e) {
      setRunning(false);
      showProgress(t('modelTest.fingerprint.startFailed', '启动失败: ') + e.message, {
        pct: 0, state: 'failed'
      });
    }
  }

  // ─── 测试表单提交 ────────────────────────────────────────────────────────
  async function onTestSubmit() {
    const channelId    = parseInt(el('fpTestChannel')?.value || '0', 10);
    const model        = (el('fpTestModel')?.value || '').trim();
    const fingerprintId = el('fpTestBaselineSelect')?.value || '';
    const iterations   = parseInt(el('fpTestIterations')?.value || DEFAULT_ITERATIONS, 10);
    const concurrency  = parseInt(el('fpTestConcurrency')?.value || DEFAULT_CONCURRENCY, 10);

    if (!channelId) { alert(t('modelTest.fingerprint.needChannel', '请选择渠道')); return; }
    if (!model)     { alert(t('modelTest.fingerprint.needModel', '请选择模型')); return; }

    const confirmMsg = t('modelTest.fingerprint.costConfirm', '将向渠道发起约 {n} 次请求，产生实际上游费用。是否继续？')
      .replace('{n}', iterations);
    if (!confirm(confirmMsg)) return;

    setRunning(true);
    hideProgress();
    const resultsDiv = el('fpResults');
    if (resultsDiv) resultsDiv.innerHTML = '';

    const body = { channel_id: channelId, model, iterations, concurrency };
    if (fingerprintId) body.fingerprint_id = parseInt(fingerprintId, 10);

    try {
      const data = await apiData('/admin/fingerprints/test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
      const jobId = data && data.job_id;
      if (!jobId) throw new Error(t('modelTest.fingerprint.startFailed', '启动失败: ') + 'empty job_id');
      startJobStream(jobId, (result) => {
        renderTestResult(result);
        loadTestHistory();
      });
    } catch (e) {
      setRunning(false);
      showProgress(t('modelTest.fingerprint.startFailed', '启动失败: ') + e.message, {
        pct: 0, state: 'failed'
      });
    }
  }

  // ─── 初始化（每次切换进指纹模式时调用） ───────────────────────────────
  function init() {
    if (!initialized) {
      initialized = true;
      _bindEvents();
    }
    _initComboboxes();
    loadFingerprints();
    loadTestHistory();
  }

  async function _initComboboxes() {
    await ensureChannels();

    // 标定：模型 combobox（全量模型）
    if (!calModelCombo) {
      calModelCombo = createCalModelCombo();
    } else {
      calModelCombo.refresh();
    }

    // 标定：渠道 combobox（按选中模型过滤）
    if (!calChannelCombo) {
      calChannelCombo = createCalChannelCombo();
    } else {
      calChannelCombo.refresh();
    }

    // 对比：模型 combobox（全量模型）
    if (!tstModelCombo) {
      tstModelCombo = createAllModelCombo('fpTestModelContainer', 'fpTestModel', onTstModelChange);
    } else {
      tstModelCombo.refresh();
    }

    // 对比：渠道 combobox（按选中模型过滤）
    if (!tstChannelCombo) {
      const modelName = el('fpTestModel')?.value || '';
      tstChannelCombo = createFilteredChannelCombo('fpTestChannelContainer', 'fpTestChannel', modelName);
    } else {
      tstChannelCombo.refresh();
    }
  }

  function _bindEvents() {
    el('fpCalibrateBtn')?.addEventListener('click', onCalibrateSubmit);
    el('fpCalibrateCancelBtn')?.addEventListener('click', cancelJob);
    el('fpTestBtn')?.addEventListener('click', onTestSubmit);
    el('fpCancelBtn')?.addEventListener('click', cancelJob);
  }

  // ─── 工具 ───────────────────────────────────────────────────────────────
  function escHtml(str) {
    return String(str).replace(/[&<>"']/g, c =>
      ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  // ─── 导出 ───────────────────────────────────────────────────────────────
  window.ModelFingerprint = { init, stopPoll };
})();
