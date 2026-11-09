// ZAI 套餐、额度展示与登录取消状态集中在此；公共模块只传入现有UI边界。
let zaiOptionsGeneration = 0;
let zaiUsageDisplayTimer = null;
let zaiOAuthStopPromise = null;

/**
 * @brief 保存单个渠道的体验优先设置，绝不提交凭证。
 * @param channelID 已保存渠道 ID。
 * @param enabled 新偏好，必须为布尔值。
 * @param fetcher 已认证请求函数。
 * @return 服务端确认的设置；请求失败时抛出错误。
 */
async function saveZAIFreeFirst(channelID, enabled, fetcher = fetchDataWithAuth) {
  if (!Number.isSafeInteger(Number(channelID)) || Number(channelID) <= 0 || typeof enabled !== 'boolean') {
    throw new Error('Invalid ZCode channel options');
  }
  return fetcher(`/admin/channels/${channelID}/zai-options`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ free_first: enabled })
  });
}

/**
 * @brief 绑定 Z.ai 即时保存开关，旧编辑器的异步结果不得写入新编辑器。
 * @param authType 当前认证类型。
 * @param credential 当前凭证视图。
 * @param channel 当前渠道快照。
 * @param onSaved 保存成功后接收服务端偏好的回调，默认为空操作。
 * @note 仅更新局部偏好，不刷新或清除其他未保存的渠道草稿。
 */
function syncZAIOptions(authType, credential, channel, onSaved = () => {}) {
  const generation = ++zaiOptionsGeneration;
  const field = document.getElementById('channelZAIFreeFirstField');
  const input = document.getElementById('channelZAIFreeFirst');
  const hint = document.getElementById('channelZAIFreeFirstHint');
  const visible = authType === 'zai_oauth';
  if (field) field.hidden = !visible;
  if (!input) return;
  const channelID = Number(channel?.id || (typeof editingChannelId !== 'undefined' ? editingChannelId : 0));
  let saved = credential?.free_first !== false;
  input.checked = saved;
  input.disabled = !visible || !channelID;
  if (hint) hint.textContent = window.t(credential?.jwt_token ? 'channels.zai.freeFirstHint' : 'channels.zai.freeFirstLoginHint');
  input.onchange = async () => {
    const desired = input.checked;
    input.disabled = true;
    try {
      const result = await saveZAIFreeFirst(channelID, desired);
      if (generation !== zaiOptionsGeneration) return;
      saved = result.free_first !== false;
      input.checked = saved;
      onSaved(saved);
    } catch (error) {
      if (generation !== zaiOptionsGeneration) return;
      input.checked = saved;
      if (window.showError) window.showError(error?.message || window.t('channels.zai.optionsFailed'));
    } finally {
      if (generation === zaiOptionsGeneration) input.disabled = false;
    }
  };
}

/**
 * @brief 格式化上游原始额度，缺失时保持横杆，不推算余额。
 * @param value 上游数值或缺失值。
 * @param unit 上游额度单位。
 * @return 本地化数值与单位；未知时返回横杆。
 */
function formatZAIQuotaAmount(value, unit) {
  if (!Number.isFinite(value)) return '—';
  const amount = value.toLocaleString(undefined, { maximumFractionDigits: 2 });
  return unit ? `${amount} ${unit === 'token' ? 'tokens' : unit}` : amount;
}

/**
 * @brief 为悬浮明细显示完整生效和截止时间，不用套餐开始时间替代权益时间。
 * @param timestamp Unix 秒。
 * @return 本地时区日期时间；缺失或非法时返回横杆。
 */
function formatZAIQuotaTime(timestamp) {
  if (!Number.isFinite(timestamp) || timestamp <= 0) return '—';
  const date = new Date(timestamp * 1000);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString(undefined, {
    year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false
  });
}

/**
 * @brief 根据已知起止时间计算显示状态，不修改接口快照或调用资格。
 * @param gift 单个套餐权益快照。
 * @param now 当前时间，毫秒；默认浏览器时间。
 * @return 当前显示状态；缺失时间时沿用上游状态，耗尽状态保持不变。
 */
function zaiGiftDisplayStatus(gift, now = Date.now()) {
  // 停用是服务端状态，不能因生效时间已过而解除。
  if (gift.status === 'inactive' || gift.status === 'expired') return gift.status;
  if (gift.expires_at > 0 && now >= gift.expires_at * 1000) return 'expired';
  if (gift.starts_at > 0) {
    if (now < gift.starts_at * 1000) return 'pending';
    if (gift.status === 'pending') {
      if (gift.granted === false) return 'ungranted';
      const exhausted = Number.isFinite(gift.remaining) ? gift.remaining <= 0
        : Number.isFinite(gift.total) && Number.isFinite(gift.used) && gift.used >= gift.total;
      return exhausted ? 'exhausted' : 'active';
    }
  }
  return gift.status;
}

/**
 * @brief 到生效或截止时间时只重绘对应额度单元格。
 * @param channelList 当前可见渠道；空列表取消定时器。
 * @param dependencies 提供 getState 和 renderCell 的额度读取与单元格渲染边界。
 * @note 只读取已有快照；切页或重绘时替换旧定时器，不查询额度、不重载列表。
 */
function scheduleZAIUsageDisplayUpdate(channelList, { getState, renderCell }) {
  clearTimeout(zaiUsageDisplayTimer);
  zaiUsageDisplayTimer = null;
  const now = Date.now();
  const candidates = [];
  for (const channel of channelList || []) {
    if (channel.auth_type !== 'zai_oauth') continue;
    const state = getState(channel.id);
    const plans = (state?.data || channel.oauth_usage)?.zai_plans;
    const times = (plans?.gifts || []).flatMap(gift => [gift.starts_at, gift.expires_at])
      .filter(time => Number.isFinite(time) && time * 1000 > now);
    if (times.length) candidates.push({ channel, at: Math.min(...times) * 1000 });
  }
  if (!candidates.length) return;
  const nextAt = Math.min(...candidates.map(item => item.at));
  // 较远的日期分段等待，避免浏览器约 24.8 天的定时器上限溢出。
  zaiUsageDisplayTimer = setTimeout(() => {
    for (const { channel, at } of candidates) {
      if (at > Date.now()) continue;
      const cell = document.getElementById(`channel-${channel.id}`)?.querySelector('.ch-col-quota');
      if (cell) cell.innerHTML = renderCell(channel);
    }
    scheduleZAIUsageDisplayUpdate(channelList, { getState, renderCell });
  }, Math.min(2147483647, Math.max(0, nextAt - now)));
}

/**
 * @brief 按套餐实例展示独立余额桶，共享模型只显示一份额度，状态按当前时间计算。
 * @param plans 已归一化的套餐快照。
 * @param refresh 公共额度刷新配置。
 * @param dependencies 提供时间、百分比、额度行、提示 ID 与转义函数的展示边界。
 * @return 套餐分组 HTML；没有体验权益时返回空字符串。
 * @details 已知正数待发放额度显示满格计划进度；悬浮明细展示待发放额度或实际剩余与总额，以及起止时间。
 */
function buildZAIPlanGroupsHtml(plans, refresh, { formatOAuthUsageResetAt, formatOAuthUsagePercent, buildOAuthUsageWindowHtml, oauthUsageTooltipID, escapeChannelRefreshText }) {
  const groups = new Map();
  for (const gift of plans?.gifts || []) {
    // 分组身份由后端提供，不能解析桶 ID，也不能按相同名称或模型合并。
    const key = gift.plan_key || gift.id;
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(gift);
  }
  // 按稳定身份排序，同名套餐和同模型桶的编号不随响应顺序改变。
  const sortedGroups = [...groups.entries()].sort(([a], [b]) => String(a).localeCompare(String(b)));
  const names = sortedGroups.map(([, gifts]) => gifts[0].name || window.t('channels.zai.trialPlan'));
  const seenNames = new Map();
  let rowIndex = 0;
  return sortedGroups.map(([, entries], groupIndex) => {
    const gifts = [...entries].sort((a, b) => String(a.id).localeCompare(String(b.id)));
    const name = names[groupIndex];
    const ordinal = (seenNames.get(name) || 0) + 1;
    seenNames.set(name, ordinal);
    const title = names.filter(value => value === name).length > 1 ? `${name} · ${ordinal}` : name;
    const labels = gifts.map(gift => {
      const models = gift.models || [];
      const modelName = models.join(', ') || gift.display_name || window.t('channels.oauth.usageQuota');
      return models.length > 1 ? window.t('channels.zai.sharedQuota', { models: modelName }) : modelName;
    });
    const seenLabels = new Map();
    const rows = gifts.map((gift, giftIndex) => {
      const displayStatus = zaiGiftDisplayStatus(gift);
      const pending = displayStatus === 'pending';
      const awaitingGrant = pending || displayStatus === 'ungranted';
      const plannedAmount = Number.isFinite(gift.total) ? gift.total : gift.grant_units;
      const plannedProgress = awaitingGrant
        && Number.isFinite(plannedAmount) && plannedAmount > 0;
      const hasBalanceProgress = displayStatus === 'active' || displayStatus === 'exhausted';
      const status = ['active', 'exhausted', 'pending', 'ungranted', 'expired', 'inactive'].includes(displayStatus)
        ? window.t(`channels.zai.giftStatus.${displayStatus}`) : String(displayStatus || '');
      const baseLabel = labels[giftIndex];
      const labelOrdinal = (seenLabels.get(baseLabel) || 0) + 1;
      seenLabels.set(baseLabel, labelOrdinal);
      const label = labels.filter(value => value === baseLabel).length > 1 ? `${baseLabel} · ${labelOrdinal}` : baseLabel;
      const remaining = Number.isFinite(gift.remaining) ? Math.max(0, gift.remaining) : null;
      // 满格仅表示确定的待发放计划，不把 grant_units 写成当前余额。
      const percent = plannedProgress ? 100
        : hasBalanceProgress && remaining !== null && Number.isFinite(gift.total) && gift.total > 0
          ? Math.min(100, remaining / gift.total * 100) : null;
      const time = formatOAuthUsageResetAt(pending ? gift.starts_at : gift.expires_at) || '—';
      const percentText = percent !== null
        ? window.t('channels.oauth.usageCompactRemaining', { percent: formatOAuthUsagePercent(percent) }) : '';
      const value = [displayStatus !== 'active' ? status : '', percentText].filter(Boolean).join(' ') || '—';
      const quotaDetails = awaitingGrant
        ? [window.t('channels.zai.pendingQuota', { amount: formatZAIQuotaAmount(plannedAmount, gift.unit_type) })]
        : [window.t('channels.management.remaining', { amount: formatZAIQuotaAmount(gift.remaining, gift.unit_type) }),
          window.t('channels.zai.totalQuota', { amount: formatZAIQuotaAmount(gift.total, gift.unit_type) })];
      return buildOAuthUsageWindowHtml({
        refresh,
        tooltipID: oauthUsageTooltipID('ch-zai-gift-tooltip', refresh.channelID, rowIndex++),
        label,
        value,
        time: window.t(pending ? 'channels.zai.startsAt' : 'channels.zai.expiresAt', { time }),
        detailLines: [title, label,
          window.t('channels.zai.startsAt', { time: formatZAIQuotaTime(gift.starts_at) }),
          window.t('channels.zai.expiresAt', { time: formatZAIQuotaTime(gift.expires_at) }), ...quotaDetails],
        ariaLabel: plannedProgress ? `${label} ${value}`
          : percent === null ? '' : window.t('channels.oauth.usageRemaining', { label, percent: formatOAuthUsagePercent(percent) }),
        percent,
        showTrack: hasBalanceProgress || plannedProgress
      });
    });
    return `<section class="ch-oauth-usage__plan" aria-label="${escapeChannelRefreshText(title)}">
      <div class="ch-oauth-usage__plan-name" title="${escapeChannelRefreshText(title)}">${escapeChannelRefreshText(title)}</div>
      ${rows.join('')}
    </section>`;
  }).join('');
}

/**
 * @brief 固定登录站点并持有同一会话直到完成；取消失败后继续原轮询。
 * @param button 授权按钮。
 * @param site 已选择的 ZCode 站点。
 * @param dependencies 提供授权配置、会话状态、代理选项、轮询和弹窗操作的边界。
 * @return 成功会话；失败或取消时返回 null。
 */
async function startZAIOAuth(button, site, { config, getActive, setActive, setCodexAuthStatus, oauthStartOptions, oauthProxyURLValue, showOAuthSession, setCodexOAuthDialogStatus, pollOAuthStatus, closeOAuthLoginDialogElement }) {
  const siteSelect = config.provider === 'zai' ? document.getElementById('zaiOAuthSite') : null;
  let resolveReady;
  let rejectReady;
  const ready = new Promise((resolve, reject) => {
    resolveReady = resolve;
    rejectReady = reject;
  });
  ready.catch(() => {});
  const flow = { state: '', provider: config.provider, site: site ?? siteSelect?.value ?? 'zai', button, cancelling: false, ready, readySettled: false };
  setActive(flow);
  if (siteSelect) {
    siteSelect.value = flow.site;
    siteSelect.disabled = true;
  }
  try {
    if (button) button.disabled = true;
    setCodexAuthStatus(window.t(`${config.i18n}.oauthStarting`));
    const startOptions = oauthStartOptions('zai', { proxyURL: oauthProxyURLValue(), site: flow.site });
    const session = await fetchDataWithAuth(`/admin/${config.provider}/oauth/start`, startOptions);
    if (!session?.url || !session?.state) throw new Error(window.t(`${config.i18n}.oauthFailed`));
    flow.state = session.state;
    flow.readySettled = true;
    resolveReady(session.state);
    // 初始化期间取消须等服务端确认；拒绝取消后仍由此执行链继续查询原会话。
    if (flow.cancelling) await flow.cancelPromise.catch(() => {});
    if (getActive() !== flow) return null;
    if (!showOAuthSession(session, config.provider)) throw new Error(window.t(`${config.i18n}.oauthFailed`));
    setCodexAuthStatus(window.t(`${config.i18n}.oauthWaiting`));
    setCodexOAuthDialogStatus(window.t(`${config.i18n}.oauthWaiting`));
    const result = await pollOAuthStatus(config.provider, session.state);
    // 取消期间到达的终态先保留，避免取消失败后丢失完成结果。
    if (flow.cancelling) await flow.cancelPromise.catch(() => {});
    if (getActive() !== flow) return null;
    closeOAuthLoginDialogElement();
    setCodexAuthStatus(window.t(`${config.i18n}.oauthComplete`), 'success');
    if (window.showSuccess) window.showSuccess(window.t(`${config.i18n}.oauthComplete`));
    await reloadChannelsList();
    return result;
  } catch (error) {
    if (!flow.readySettled) {
      flow.readySettled = true;
      rejectReady(error);
    }
    if (flow.cancelling) await flow.cancelPromise.catch(() => {});
    if (getActive() !== flow) return null;
    const unavailable = config.provider === 'zai' && /unavailable/i.test(error?.message);
    const message = unavailable
      ? window.t(`${config.i18n}.oauthUnavailable`)
      : (error?.message || window.t(`${config.i18n}.oauthFailed`));
    setCodexAuthStatus(message, 'error');
    setCodexOAuthDialogStatus(message, 'error');
    if (window.showError) window.showError(message);
    return null;
  } finally {
    // 取消期间由 stop 持有会话，等待服务端确认后再解锁。
    if (getActive() === flow && !flow.cancelling) {
      setActive(null);
      if (button) button.disabled = false;
      if (siteSelect) siteSelect.disabled = false;
    }
  }
}

/**
 * @brief 等待取消确认，失败时由原登录执行链恢复状态查询。
 * @param flow 当前登录会话；为空时只处理弹窗清理。
 * @param options 是否在取消成功后关闭弹窗。
 * @param dependencies 提供当前会话读写、服务端取消和弹窗状态操作的边界。
 * @return 取消完成的 Promise；服务端拒绝或传输失败时抛出原错误。
 */
async function stopZAIOAuth(flow, options, { getActive, setActive, cancelOAuth, setCodexOAuthDialogStatus, closeOAuthLoginDialogElement, setCodexAuthStatus }) {
  const closeDialog = options.closeDialog !== false;
  if (zaiOAuthStopPromise) return zaiOAuthStopPromise;

  const operation = (async () => {
    if (flow) {
      flow.cancelling = true;
      setCodexOAuthDialogStatus(window.t('channels.oauth.cancelling'));
      if (!flow.state && flow.ready) {
        try {
          await flow.ready;
        } catch {
          // 初始化失败没有服务端会话，仍需走下面的本地清理。
        }
      }
      try {
        if (flow.state) await cancelOAuth(flow.provider, flow.state);
      } catch (error) {
        flow.cancelling = false;
        throw error;
      }
      if (getActive() !== flow) return;
      setActive(null);
      if (flow.button) flow.button.disabled = false;
      if (flow.provider === 'zai') {
        const site = document.getElementById('zaiOAuthSite');
        if (site) site.disabled = false;
      }
    }
    if (closeDialog) {
      closeOAuthLoginDialogElement();
      setCodexAuthStatus('');
      setCodexOAuthDialogStatus('');
    }
  })();

  zaiOAuthStopPromise = operation;
  if (flow) flow.cancelPromise = operation;
  try {
    return await operation;
  } finally {
    if (zaiOAuthStopPromise === operation) zaiOAuthStopPromise = null;
  }
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { saveZAIFreeFirst, syncZAIOptions, formatZAIQuotaAmount, formatZAIQuotaTime, zaiGiftDisplayStatus, scheduleZAIUsageDisplayUpdate, buildZAIPlanGroupsHtml, startZAIOAuth, stopZAIOAuth };
}
