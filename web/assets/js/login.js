(function() {
    const form = document.getElementById('login-form');
    const errorMessage = document.getElementById('error-message');
    const errorText = document.getElementById('error-text');
    const loginButton = document.getElementById('login-button');
    const passwordInput = document.getElementById('password');
    const apiTokenInput = document.getElementById('api-token');
    const adminGroup = document.getElementById('admin-credential-group');
    const apiTokenGroup = document.getElementById('api-token-credential-group');
    const loginTitle = document.getElementById('login-title');
    const loginHint = document.getElementById('login-hint');
    let loginMode = 'admin';

    function showError(message) {
      if (window.showError) try { window.showError(message); } catch (_) {}
      errorText.textContent = message;
      errorMessage.style.display = 'flex';
      
      // 添加摇晃动画
      errorMessage.style.animation = 'none';
      errorMessage.offsetHeight; // 触发重绘
      errorMessage.style.animation = 'slideInUp 0.3s ease-out';
    }

    function hideError() {
      errorMessage.style.display = 'none';
    }

    function setLoading(loading) {
      loginButton.classList.toggle('loading', loading);
      loginButton.disabled = loading;
      passwordInput.disabled = loading;
      apiTokenInput.disabled = loading;
    }

    function setLoginMode(mode) {
      loginMode = mode === 'api_token' ? 'api_token' : 'admin';
      const tokenMode = loginMode === 'api_token';
      adminGroup.classList.toggle('hidden', tokenMode);
      apiTokenGroup.classList.toggle('hidden', !tokenMode);
      passwordInput.required = !tokenMode;
      apiTokenInput.required = tokenMode;
      document.querySelectorAll('[data-login-mode]').forEach((tab) => {
        const active = tab.dataset.loginMode === loginMode;
        tab.classList.toggle('active', active);
        tab.setAttribute('aria-selected', active ? 'true' : 'false');
      });
      const titleKey = tokenMode ? 'login.apiTokenLogin' : 'login.adminLogin';
      const hintKey = tokenMode ? 'login.apiTokenHint' : 'login.passwordHint';
      loginTitle.dataset.i18n = titleKey;
      loginHint.dataset.i18n = hintKey;
      loginTitle.textContent = window.t(titleKey);
      loginHint.textContent = window.t(hintKey);
      hideError();
      (tokenMode ? apiTokenInput : passwordInput).focus();
    }

    // 表单提交处理
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      hideError();
      setLoading(true);

      const credentialInput = loginMode === 'api_token' ? apiTokenInput : passwordInput;
      const credential = loginMode === 'api_token' ? credentialInput.value.trim() : credentialInput.value;
      const payload = window.WebAuth.buildLoginPayload(loginMode, credential);

      try {
        const resp = await fetchAPI('/login', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
          },
          body: JSON.stringify(payload),
        });

        if (resp.success) {
          const data = resp.data || {};

          window.WebAuth.storeWebSession(localStorage, data);

          // 登录成功，添加成功动画
          loginButton.style.background = 'linear-gradient(135deg, var(--success-500), var(--success-600))';

          setTimeout(() => {
            const urlParams = new URLSearchParams(window.location.search);
            const redirect = window.WebAuth.getSafeRedirectPath(urlParams.get('redirect'), window.location.origin);
            window.location.href = redirect;
          }, 500);
        } else {
          showError(resp.error || '凭据无效，请重试');

          // 添加输入框摇晃动画
          credentialInput.style.animation = 'none';
          credentialInput.offsetHeight;
          credentialInput.style.animation = 'shake 0.5s ease-in-out';

          setTimeout(() => {
            credentialInput.style.animation = '';
          }, 500);
        }
      } catch (error) {
        console.error('Login error:', error);
        showError('网络连接错误，请检查网络后重试');
      } finally {
        setLoading(false);
      }
    });

    // 输入框焦点处理
    passwordInput.addEventListener('focus', hideError);
    apiTokenInput.addEventListener('focus', hideError);
    document.querySelectorAll('[data-login-mode]').forEach((tab) => {
      tab.addEventListener('click', () => setLoginMode(tab.dataset.loginMode));
    });
    
    // 键盘快捷键
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        hideError();
      }
    });

    // 检查URL参数中的错误信息
    const urlParams = new URLSearchParams(window.location.search);
    const errorParam = urlParams.get('error');
    if (errorParam) {
      showError(errorParam);
    }

    // 页面加载完成后的初始化
    document.addEventListener('DOMContentLoaded', function() {
      if (window.i18n) window.i18n.translatePage();
      setLoginMode('admin');
      // 聚焦到密码输入框
      setTimeout(() => {
        passwordInput.focus();
      }, 500);

      // 添加输入框摇晃动画关键帧
      const style = document.createElement('style');
      style.textContent = `
        @keyframes shake {
          0%, 100% { transform: translateX(0); }
          10%, 30%, 50%, 70%, 90% { transform: translateX(-8px); }
          20%, 40%, 60%, 80% { transform: translateX(8px); }
        }
      `;
      document.head.appendChild(style);
    });
})();
