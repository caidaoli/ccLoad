(function () {
  'use strict';

  function translate(translateFn, key, fallback, params) {
    if (typeof translateFn === 'function') return translateFn(key, fallback, params);
    let text = fallback;
    Object.entries(params || {}).forEach(([name, value]) => {
      text = text.replace(new RegExp(`\\{${name}\\}`, 'g'), String(value));
    });
    return text;
  }

  function modelName(value) {
    return typeof value === 'string' ? value.trim() : '';
  }

  function summarizeModelVerification(verification, translateFn) {
    if (!verification || typeof verification !== 'object') return null;

    const verdict = verification.verdict;
    const source = verification.source;
    const labels = [];
    let verdictClass = 'unverified';

    if (verdict === 'mismatch') {
      labels.push(translate(translateFn, 'modelTest.verification.mismatch', '模型名不一致'));
      verdictClass = 'mismatch';
    } else if (verdict === 'consistent') {
      labels.push(translate(translateFn, 'modelTest.verification.consistent', '模型名一致（未证实）'));
      verdictClass = 'consistent';
    } else {
      labels.push(translate(translateFn, 'modelTest.verification.unverified', '未验证'));
    }

    if (source === 'likely_web_bridge') {
      labels.push(translate(translateFn, 'modelTest.verification.webBridge', '疑似网页桥接'));
    }

    const lines = [];
    const claimedModel = modelName(verification.claimed_model);
    const effectiveModel = modelName(verification.effective_model);
    const reportedModel = modelName(verification.reported_model);
    if (claimedModel) {
      lines.push(translate(translateFn, 'modelTest.verification.claimed', '请求模型：{model}', { model: claimedModel }));
    }
    if (effectiveModel) {
      lines.push(translate(translateFn, 'modelTest.verification.effective', '实际上游模型：{model}', { model: effectiveModel }));
    }
    if (reportedModel) {
      lines.push(translate(translateFn, 'modelTest.verification.reported', '响应声明模型：{model}', { model: reportedModel }));
    }
    if (verification.model_rewritten) {
      lines.push(translate(translateFn, 'modelTest.verification.rewritten', '渠道配置改写了模型名'));
    }

    const catalog = verification.catalog;
    if (catalog && typeof catalog === 'object' && catalog.attempted) {
      if (catalog.available) {
        lines.push(translate(translateFn, 'modelTest.verification.catalogAvailable', '模型目录：{count} 项', { count: Number(catalog.model_count) || 0 }));
        lines.push(catalog.effective_model_listed
          ? translate(translateFn, 'modelTest.verification.effectiveListed', '实际上游模型在目录中')
          : translate(translateFn, 'modelTest.verification.effectiveNotListed', '实际上游模型不在目录中'));
        if (reportedModel) {
          lines.push(catalog.reported_model_listed
            ? translate(translateFn, 'modelTest.verification.reportedListed', '响应声明模型在目录中')
            : translate(translateFn, 'modelTest.verification.reportedNotListed', '响应声明模型不在目录中'));
        }
      } else {
        lines.push(translate(translateFn, 'modelTest.verification.catalogUnavailable', '模型目录不可用'));
      }
    }
    if (source === 'likely_web_bridge') {
      lines.push(translate(translateFn, 'modelTest.verification.webBridgeDetail', '检测到 ChatGPT Web 后端接口特征'));
    }
    lines.push(translate(translateFn, 'modelTest.verification.limitation', '响应元数据和模型目录可被中转伪造，不能证明底层模型。'));

    return {
      className: `model-verification--${verdictClass}${source === 'likely_web_bridge' ? ' model-verification--web-bridge' : ''}`,
      label: labels.join(' · '),
      title: lines.join('\n')
    };
  }

  function renderModelVerification(responseCell, verification, translateFn) {
    if (!responseCell || typeof responseCell.querySelectorAll !== 'function') return;
    responseCell.querySelectorAll('.model-verification').forEach((badge) => badge.remove());

    const summary = summarizeModelVerification(verification, translateFn);
    if (!summary || typeof document === 'undefined') return;

    const badge = document.createElement('span');
    badge.className = `model-verification ${summary.className}`;
    badge.textContent = summary.label;
    badge.title = summary.title;
    badge.setAttribute('aria-label', summary.title);
    if (responseCell.textContent.trim()) {
      responseCell.append(document.createTextNode(' '));
    }
    responseCell.append(badge);
  }

  const api = {
    summarizeModelVerification,
    renderModelVerification
  };

  if (typeof window !== 'undefined') {
    window.ModelVerificationUI = api;
  }
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
})();
