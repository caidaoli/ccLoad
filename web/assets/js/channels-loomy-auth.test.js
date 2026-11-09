const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');

/**
 * @brief 装载实际公共额度与 Loomy 模块构造隔离测试环境。
 * @return 返回可调用实际业务函数的 VM 上下文。
 * @throw Error 源文件读取或脚本求值失败时。
 * @note 仅替换页面绘制依赖，保留公共状态管理。
 */
function loadLoomy() {
  const context = vm.createContext({
    window: { t: key => key }, channels: [],
    AbortController,
    document: { querySelector: () => null }
  });
  // 与页面相同，加载共用额度流程，避免测试自行构造另一套状态。
  for (const file of ['channels-zai.js', 'channels-codex-auth.js', 'channels-loomy-auth.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, file), 'utf8'), context);
  }
  vm.runInContext('Object.assign(globalThis, { oauthUsageStateByChannelID }); rerenderOAuthUsage = () => {};', context);
  return context;
}

/**
 * @brief 装载实际列表刷新流程和最小登录 DOM 替身。
 * @return 返回 ctx、elements 和 pageEvents，用于驱动登录与取消行为。
 * @throw Error 源文件读取、求值或初始化失败时。
 * @note 本地模拟网络响应，不发送真实短信。
 */
function loadLoomyLogin() {
  const ctx = loadLoomy();
  const elements = new Map();
  const pageEvents = new Map();
  ctx.window.addEventListener = (name, handler) => pageEvents.set(name, handler);
  /**
   * @brief 创建并登记可触发事件的登录表单节点替身。
   * @param id 用于按页面实际节点查找的 ID。
   * @return 返回登记在 elements 中的节点替身。
   * @note 将该节点加入测试元素映射。
   */
  const element = id => {
    const handlers = new Map();
    const node = {
      dataset: {}, value: '', disabled: false, hidden: false,
      focus() {}, reset() {}, reportValidity: () => true,
      addEventListener: (event, handler) => handlers.set(event, handler),
      emit: (event, payload) => handlers.get(event)?.(payload),
      showModal() { this.open = true; },
      close() { this.open = false; this.emit('close'); }
    };
    elements.set(id, node);
    return node;
  };
  for (const id of ['loomyLoginDialog', 'loomyLoginForm', 'loomyPhone', 'loomyProxy', 'loomyCode',
    'loomySendCode', 'loomyVerify', 'loomyLoginStatus', 'loomyLoginClose', 'open']) element(id);
  Object.assign(ctx, {
    URLSearchParams, console,
    document: {
      getElementById: id => elements.get(id),
      querySelectorAll: () => [elements.get('open')]
    },
    channelsReadURL: admin => admin,
    filters: {}, channelsSort: { key: 'priority', order: 'desc' },
    channelsCurrentPage: 1, channelsPageSize: 20, channelStatsRange: 'today',
    allAvailableChannelNames: ['old'], allAvailableModels: ['old-model'],
    filterChannels() {},
    fetchAPIWithAuth: async () => ({ success: true, data: [{ id: 8, name: 'Loomy-new' }], count: 1 }),
    fetchDataWithAuth: async url => {
      if (url.endsWith('/send-code')) return { login_id: 'verified-challenge' };
      if (url.endsWith('/verify')) return { channel_id: 8 };
      if (url.startsWith('/admin/channels/filter-options?')) {
        return { channel_names: ['old', 'Loomy-new'], models: ['old-model', 'new-chat'] };
      }
      throw new Error(`Unexpected request ${url}`);
    }
  });
  // 加载实际列表模块，验证可用搜索数据，而不是断言调用了某个 helper。
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'channels-data.js'), 'utf8'), ctx);
  ctx.setupLoomyActions();
  return { ctx, elements, pageEvents };
}

test('SMS login refreshes saved channels and the complete name/model search options', async () => {
  const { ctx, elements } = loadLoomyLogin();
  elements.get('open').emit('click');
  elements.get('loomyPhone').value = '13800138000';
  await elements.get('loomySendCode').emit('click');
  assert.equal(elements.get('loomyVerify').disabled, false);
  elements.get('loomyCode').value = '123456';
  await elements.get('loomyLoginForm').emit('submit', { preventDefault() {} });
  assert.equal(elements.get('loomyLoginDialog').open, false);
  assert.equal(ctx.channels[0].name, 'Loomy-new');
  assert.deepEqual(Array.from(ctx.allAvailableChannelNames), ['old', 'Loomy-new']);
  assert.deepEqual(Array.from(ctx.allAvailableModels), ['old-model', 'new-chat']);
});

for (const stage of ['send-code', 'verify']) {
  for (const exit of ['close', 'pagehide']) {
    test(`SMS ${stage} is canceled on ${exit} and ignores a late response`, async () => {
      const { ctx, elements, pageEvents } = loadLoomyLogin();
      let finish, signal;
      let reloads = 0;
      ctx.reloadChannelsList = async () => { reloads++; };
      ctx.fetchDataWithAuth = async (url, options) => {
        if (!url.endsWith(stage)) return { login_id: 'verified-challenge' };
        signal = options.signal;
        return new Promise(resolve => { finish = resolve; });
      };
      elements.get('open').emit('click');
      elements.get('loomyPhone').value = '13800000001';
      let pending;
      if (stage === 'verify') {
        await elements.get('loomySendCode').emit('click');
        elements.get('loomyCode').value = '123456';
        pending = elements.get('loomyLoginForm').emit('submit', { preventDefault() {} });
      } else {
        pending = elements.get('loomySendCode').emit('click');
      }
      assert.ok(signal, 'login request must support cancellation');
      assert.equal(signal.aborted, false);
      if (exit === 'close') elements.get('loomyLoginClose').emit('click');
      else pageEvents.get('pagehide')();
      assert.equal(signal.aborted, true);
      // 即使底层在取消后仍返回结果，也不能给已关闭的流程复活挑战或刷新列表。
      finish(stage === 'verify' ? { channel_id: 8 } : { login_id: 'late-challenge' });
      await pending;
      assert.equal(reloads, 0);
      assert.equal(elements.get('loomyCode').value, '');
      assert.equal(elements.get('loomyVerify').disabled, true);
      elements.get('open').emit('click');
      ctx.fetchDataWithAuth = async () => ({ login_id: 'new-challenge' });
      await elements.get('loomySendCode').emit('click');
      assert.equal(elements.get('loomyVerify').disabled, false, 'new login can send another code');
    });
  }
}

test('Loomy check-in succeeds without balance and retains the previous sample', async () => {
  const ctx = loadLoomy();
  ctx.channels.push({ id: 1, auth_type: 'loomy_session' });
  const usage = { provider: 'loomy', windows: [], loomy_credits: { balance: 10 } };
  ctx.oauthUsageStateByChannelID.set(1, { status: 'ready', data: usage });
  const result = await ctx.checkInLoomy(1, async (url, options) => {
    assert.equal(url, '/admin/channels/1/loomy-checkin');
    assert.equal(options.method, 'POST');
    // 请求等待期间刷新列表会替换对象，结果仍须更新当前可见渠道。
    ctx.channels = [{ id: 1, auth_type: 'loomy_session', oauth_usage: usage }];
    return { status: 'success', day: '2026-11-05', onboarding: 'pending', warnings: ['balance unavailable'] };
  }, { reload: false });
  assert.equal(result.status, 'success');
  const state = ctx.oauthUsageStateByChannelID.get(1);
  assert.equal(state.checkin_status, 'ready');
  assert.equal(state.checkin_day, '2026-11-05');
  assert.equal(ctx.channels[0].loomy_last_checkin_day, '2026-11-05');
  assert.equal(state.data, usage);
  assert.match(state.checkin_notice, /tasksPending/);
  assert.match(state.checkin_notice, /balance unavailable/);
});

test('Loomy ignores an older check-in response and retains the latest state', async () => {
  const ctx = loadLoomy();
  let finish;
  const older = ctx.checkInLoomy(1, () => new Promise(resolve => { finish = resolve; }), { reload: false });
  await ctx.checkInLoomy(1, async () => ({ status: 'already_checked', day: '2026-11-05', onboarding: 'completed' }), { reload: false });
  finish({ status: 'success', day: '2026-11-04', onboarding: 'pending' });
  await older;
  assert.equal(ctx.oauthUsageStateByChannelID.get(1).checkin_result, 'already_checked');
  assert.equal(ctx.oauthUsageStateByChannelID.get(1).checkin_day, '2026-11-05');
  assert.match(ctx.oauthUsageStateByChannelID.get(1).checkin_notice, /tasksCompleted/);
});

test('Loomy failed daily check-in surfaces the error without erasing balance', async () => {
  const ctx = loadLoomy();
  const usage = { windows: [] };
  ctx.oauthUsageStateByChannelID.set(1, { status: 'ready', data: usage, checkin_day: '2026-11-05', checkin_notice: 'old success' });
  await assert.rejects(ctx.checkInLoomy(1, async () => ({ status: 'failed', warnings: ['expired'] }), { reload: false }), /expired/);
  assert.equal(ctx.oauthUsageStateByChannelID.get(1).checkin_status, 'error');
  assert.equal(ctx.oauthUsageStateByChannelID.get(1).data, usage);
  assert.equal(ctx.oauthUsageStateByChannelID.get(1).checkin_day, '2026-11-05');
  assert.equal(ctx.oauthUsageStateByChannelID.get(1).checkin_notice, '');
});

test('balance refresh cannot supersede an in-flight Loomy check-in', async () => {
  const ctx = loadLoomy();
  let finish;
  const checkin = ctx.checkInLoomy(1, () => new Promise(resolve => { finish = resolve; }), { reload: false });
  let refreshCalls = 0;
  /**
   * @brief 捕获签到进行期间不应发生的余额刷新请求。
   * @return 始终拒绝的 Promise，表示额度刷新越过了变更互斥。
   * @throw Error 被调用时立即抛错，使互斥回归可被测试发现。
   * @note 每次调用递增请求计数。
   */
  const fetcher = async () => { refreshCalls++; throw new Error('must not refresh while checking in'); };
  const batch = await ctx.refreshOAuthUsageBatch([1], fetcher, { reload: false });
  await ctx.refreshOAuthUsage(1, fetcher, { reload: false });
  const automatic = await ctx.maybeAutoRefreshActiveChannelUsage([1], fetcher);
  assert.equal(batch.total, 0);
  assert.equal(automatic, null);
  assert.equal(refreshCalls, 0);
  finish({ status: 'success', day: '2026-11-05', onboarding: 'pending', warnings: ['tasks pending'] });
  await checkin;
  assert.equal(ctx.getOAuthUsageState(1).checkin_result, 'success');
  assert.equal(ctx.getOAuthUsageState(1).checkin_day, '2026-11-05');
  assert.match(ctx.getOAuthUsageState(1).checkin_notice, /tasks pending/);
});
