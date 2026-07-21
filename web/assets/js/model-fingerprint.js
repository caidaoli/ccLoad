/**
 * model-fingerprint.js — 指纹对比模式 UI
 *
 * 依赖（在 model-test.html 中于本脚本之前加载）：
 *   - ui.js          → fetchDataWithAuth / fetchAPIWithAuth / i18nText
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
  const POLL_INTERVAL_MS    = 1000;

  // ─── 状态 ───────────────────────────────────────────────────────────────
  let fingerprints  = [];   // GET /admin/fingerprints 返回列表
  let activeJobId   = null;
  let pollTimer     = null;
  let initialized   = false;

  // ─── DOM 引用（延迟获取）────────────────────────────────────────────────
  function el(id) { return document.getElementById(id); }

  // ─── i18n 辅助 ──────────────────────────────────────────────────────────
  function t(key, fallback) {
    return (typeof window.i18nText === 'function')
      ? window.i18nText(key, fallback || key)
      : (fallback || key);
  }

  // ─── API 调用 ────────────────────────────────────────────────────────────
  async function apiFetch(url, options) {
    return window.fetchAPIWithAuth(url, options);
  }

  async function apiData(url, options) {
    return window.fetchDataWithAuth(url, options);
  }

  // ─── 渠道/模型 select 渲染 ──────────────────────────────────────────────
  function buildChannelOptions(selectEl) {
    const channels = (window.channelsList || []);
    selectEl.innerHTML = '<option value="">' + t('modelTest.fingerprint.selectChannel', '选择渠道') + '</option>';
    channels.forEach(ch => {
      const opt = document.createElement('option');
      opt.value = ch.id;
      opt.textContent = ch.name + ' (#' + ch.id + ')';
      selectEl.appendChild(opt);
    });
  }

  function buildModelOptions(selectEl, channelId) {
    const channels = (window.channelsList || []);
    const ch = channels.find(c => String(c.id) === String(channelId));
    const models = (ch && ch.models) ? ch.models : [];
    selectEl.innerHTML = '<option value="">' + t('modelTest.fingerprint.selectModel', '选择模型') + '</option>';
    models.forEach(m => {
      const name = (typeof m === 'string') ? m : (m.model || m.name || '');
      if (!name) return;
      const opt = document.createElement('option');
      opt.value = name;
      opt.textContent = name;
      selectEl.appendChild(opt);
    });
  }

  function wireChannelModelSync(channelSel, modelSel) {
    channelSel.addEventListener('change', () => {
      buildModelOptions(modelSel, channelSel.value);
    });
  }

  // ─── 基准列表渲染 ──────────────────────────────────────────────────────
  function renderBaselineTable() {
    const tbody = el('fpBaselineTbody');
    const select = el('fpTestBaselineSelect');
    if (!tbody) return;

    tbody.innerHTML = '';
    // 更新 test 表单里的基准 select
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
      const createdAt = fp.created_at ? new Date(fp.created_at).toLocaleString() : '-';
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

    // 删除按钮
    tbody.querySelectorAll('.fp-delete-btn').forEach(btn => {
      btn.addEventListener('click', () => deleteFingerprint(btn.dataset.id));
    });
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

  // ─── 进度 UI ────────────────────────────────────────────────────────────
  function showProgress(text) {
    const p = el('fpProgress');
    if (p) { p.textContent = text; p.classList.remove('hidden'); }
  }

  function hideProgress() {
    const p = el('fpProgress');
    if (p) p.classList.add('hidden');
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

  // ─── 结果渲染 ────────────────────────────────────────────────────────────
  function renderCalibrateResult(result) {
    const div = el('fpResults');
    if (!div) return;
    div.innerHTML = '';

    if (!result) {
      div.textContent = t('modelTest.fingerprint.noResult', '无结果');
      return;
    }

    // result 是 ModelFingerprint 对象
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

    // 统计摘要
    if (result.stats || result.sample_count != null) {
      const summary = document.createElement('div');
      summary.className = 'fp-result-summary';
      summary.textContent = t('modelTest.fingerprint.sampleCount', '样本') + ': '
        + (result.sample_count != null ? result.sample_count : '-');
      div.appendChild(summary);
    }

    div.classList.remove('hidden');
  }

  // UI-only thresholds from design doc (no routing impact).
  function scoreHint(score) {
    if (score == null || typeof score !== 'number') return '';
    if (score >= 0.85) return t('modelTest.fingerprint.hint.high', '高度一致');
    if (score >= 0.65) return t('modelTest.fingerprint.hint.medium', '中等一致（建议加大采样复核）');
    return t('modelTest.fingerprint.hint.low', '明显不一致（疑似换模/掺假）');
  }

  // ─── Job 轮询 ────────────────────────────────────────────────────────────
  function startPoll(jobId, onComplete) {
    stopPoll();
    activeJobId = jobId;
    showProgress(t('modelTest.fingerprint.running', '运行中…'));

    pollTimer = setInterval(async () => {
      try {
        const job = await apiData('/admin/fingerprints/jobs/' + jobId);
        if (!job) return;

        const status = job.status;
        // 更新进度显示
        if (job.progress != null) {
          let progressText;
          if (job.progress !== null && typeof job.progress === 'object') {
            const p = job.progress;
            progressText = t('modelTest.fingerprint.progress', '进度') + ': '
              + (p.success != null ? p.success + ' ok' : '')
              + (p.failed != null ? ' / ' + p.failed + ' fail' : '')
              + (p.done != null && p.total != null ? ' / ' + p.done + '/' + p.total : '')
              + ' — ' + status;
          } else {
            const pct = typeof job.progress === 'number' ? job.progress : 0;
            progressText = t('modelTest.fingerprint.progress', '进度') + ': ' + pct + '% — ' + status;
          }
          showProgress(progressText);
        }

        if (status === 'succeeded' || status === 'failed' || status === 'cancelled') {
          stopPoll();
          setRunning(false);
          if (status === 'succeeded') {
            hideProgress();
            onComplete(job.result);
          } else if (status === 'cancelled') {
            showProgress(t('modelTest.fingerprint.cancelled', '已取消'));
          } else {
            showProgress(t('modelTest.fingerprint.failed', '失败: ') + (job.error || ''));
          }
        }
      } catch (e) {
        // 网络抖动时静默忽略，下次再试
      }
    }, POLL_INTERVAL_MS);
  }

  function stopPoll() {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  }

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
      startPoll(jobId, (result) => {
        renderCalibrateResult(result);
        loadFingerprints();
      });
    } catch (e) {
      setRunning(false);
      showProgress(t('modelTest.fingerprint.startFailed', '启动失败: ') + e.message);
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
      startPoll(jobId, (result) => {
        renderTestResult(result);
      });
    } catch (e) {
      setRunning(false);
      showProgress(t('modelTest.fingerprint.startFailed', '启动失败: ') + e.message);
    }
  }

  // ─── 初始化（每次切换进指纹模式时调用） ───────────────────────────────
  function init() {
    if (!initialized) {
      initialized = true;
      _bindEvents();
    }
    // 每次进入时刷新渠道列表和基准列表
    _refreshChannelSelects();
    loadFingerprints();
  }

  function _refreshChannelSelects() {
    const calChannel = el('fpCalibrateChannel');
    const calModel   = el('fpCalibrateModel');
    const tstChannel = el('fpTestChannel');
    const tstModel   = el('fpTestModel');
    if (calChannel) { buildChannelOptions(calChannel); buildModelOptions(calModel, calChannel.value); }
    if (tstChannel) { buildChannelOptions(tstChannel); buildModelOptions(tstModel, tstChannel.value); }
  }

  function _bindEvents() {
    // 渠道→模型联动
    const calChannel = el('fpCalibrateChannel');
    const calModel   = el('fpCalibrateModel');
    const tstChannel = el('fpTestChannel');
    const tstModel   = el('fpTestModel');
    if (calChannel && calModel) wireChannelModelSync(calChannel, calModel);
    if (tstChannel && tstModel) wireChannelModelSync(tstChannel, tstModel);

    // 提交按钮
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
