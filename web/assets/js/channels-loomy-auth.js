/**
 * @brief 绑定一次性短信登录交互并隔离已取消操作的迟到响应。
 * @note 缺少对话框或已绑定时跳过；关闭及 pagehide 取消请求，登录成功后刷新渠道及筛选全集。
 */
function setupLoomyActions() {
  const dialog = document.getElementById('loomyLoginDialog');
  if (!dialog || dialog.dataset.bound === 'true') return;
  dialog.dataset.bound = 'true';
  const form = document.getElementById('loomyLoginForm');
  const phone = document.getElementById('loomyPhone');
  const proxy = document.getElementById('loomyProxy');
  const code = document.getElementById('loomyCode');
  const send = document.getElementById('loomySendCode');
  const submit = document.getElementById('loomyVerify');
  const status = document.getElementById('loomyLoginStatus');
  let loginID = '';
  let generation = 0;
  let busy = false;
  let controller = null;
  /**
   * @brief 在身份输入变化或挑战消费后清除本地登录挑战。
   * @note 清空挑战 ID 并禁用提交，防止旧手机号或代理的挑战被复用。
   */
  const invalidate = () => { loginID = ''; submit.disabled = true; };
  /**
   * @brief 在短信发送或验证期间冻结身份输入并同步提交可用性。
   * @param value 是否存在进行中的登录请求。
   * @note 更新输入与按钮禁用态；即使空闲，没有挑战也不能提交。
   */
  const setBusy = value => {
    busy = value;
    send.disabled = value;
    submit.disabled = value || !loginID;
    phone.disabled = value;
    proxy.disabled = value;
    code.disabled = value;
  };
  /**
   * @brief 取消当前登录请求并使迟到回包失去更新界面的资格。
   * @note 递增操作代次、终止请求、清空挑战与验证码并恢复输入状态。
   */
  const cancelLogin = () => {
    generation++;
    controller?.abort();
    controller = null;
    invalidate();
    setBusy(false);
    code.value = '';
  };
  document.querySelectorAll('[data-action="open-loomy-login"]').forEach(button => {
    button.addEventListener('click', () => {
      if (typeof isTokenChannelsReadOnly === 'function' && isTokenChannelsReadOnly()) return;
      cancelLogin();
      form.reset();
      invalidate();
      setBusy(false);
      setOAuthStatus(status, '');
      dialog.showModal();
      phone.focus();
    });
  });
  document.getElementById('loomyLoginClose').addEventListener('click', () => dialog.close());
  dialog.addEventListener('close', cancelLogin);
  window.addEventListener?.('pagehide', cancelLogin);
  phone.addEventListener('input', invalidate);
  proxy.addEventListener('input', invalidate);
  send.addEventListener('click', async () => {
    if (busy || !phone.reportValidity()) return;
    const operation = ++generation;
    controller = new AbortController();
    invalidate();
    setBusy(true);
    setOAuthStatus(status, window.t('channels.loomy.sending'));
    try {
      const result = await fetchDataWithAuth('/admin/loomy/login/send-code', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        signal: controller.signal,
        body: JSON.stringify({ phone: phone.value, proxy_url: proxy.value.trim() })
      });
      if (operation !== generation) return;
      if (!result?.login_id) throw new Error(window.t('channels.loomy.invalidResponse'));
      loginID = result.login_id;
      setOAuthStatus(status, window.t('channels.loomy.codeSent'), 'success');
    } catch (error) {
      if (operation === generation) setOAuthStatus(status, error.message, 'error');
    } finally {
      if (operation === generation) { controller = null; setBusy(false); code.focus(); }
    }
  });
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (busy || !loginID || !form.reportValidity()) return;
    const operation = ++generation;
    controller = new AbortController();
    setBusy(true);
    setOAuthStatus(status, window.t('channels.loomy.signingIn'));
    try {
      const result = await fetchDataWithAuth('/admin/loomy/login/verify', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        signal: controller.signal,
        body: JSON.stringify({ login_id: loginID, code: code.value.trim() })
      });
      if (operation !== generation) return;
      if (!result?.channel_id) throw new Error(window.t('channels.loomy.invalidResponse'));
      controller = null;
      dialog.close();
      // 新增账号后同步刷新筛选全集，与其他渠道创建流程使用同一入口。
      await reloadChannelsList({ refreshUsage: false });
      window.showSuccess?.(window.t('channels.loomy.loginSuccess'));
    } catch (error) {
      if (operation === generation) {
        invalidate();
        setOAuthStatus(status, `${error.message} ${window.t('channels.loomy.resend')}`, 'error');
      }
    } finally {
      if (operation === generation) { controller = null; setBusy(false); }
    }
  });
}

/**
 * @brief 通过公共签到流程领取 Loomy 每日积分并允许附属余额暂缺。
 * @param channelID 有效的渠道 ID。
 * @param fetcher 返回已解包业务结果的认证请求函数。
 * @param options 签到后的列表刷新选项。
 * @return Promise：返回包含每日签到、新手任务状态及可选余额的结果。
 * @throw Error 渠道 ID、请求或签到状态无效，以及后续刷新失败时。
 * @note 复用公共签到状态供余额刷新避让，并按代次保护结果，不因余额失败撤销已成功的签到。
 */
async function checkInLoomy(channelID, fetcher = fetchDataWithAuth, options = {}) {
  return checkInOAuthChannel(channelID, {
    provider: 'loomy', label: 'Loomy', usageOptional: true, notice: loomyCheckinMessage
  }, fetcher, options);
}

/**
 * @brief 组合每日签到、新手任务和告警的本地化结果提示。
 * @param result 已经校验签到状态的业务结果。
 * @return 返回可直接显示的纯文本提示，不改变业务成功状态。
 */
function loomyCheckinMessage(result) {
  // 共用签到流程和布局，结果文案使用实际渠道名称。
  const parts = [window.t(result.status === 'already_checked' ? 'channels.loomy.alreadyCheckedIn' : 'channels.loomy.checkinSuccess')];
  if (result.onboarding === 'completed') parts.push(window.t('channels.loomy.tasksCompleted'));
  if (result.onboarding === 'pending') parts.push(window.t('channels.loomy.tasksPending'));
  if (Array.isArray(result.warnings)) parts.push(...result.warnings.filter(Boolean));
  return parts.join(' · ');
}
