(function() {
    const form = document.getElementById('login-form');
    const errorMessage = document.getElementById('error-message');
    const errorText = document.getElementById('error-text');
    const loginButton = document.getElementById('login-button');
    const passwordInput = document.getElementById('password');

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
      if (loading) {
        loginButton.classList.add('loading');
        loginButton.disabled = true;
        passwordInput.disabled = true;
      } else {
        loginButton.classList.remove('loading');
        loginButton.disabled = false;
        passwordInput.disabled = false;
      }
    }

    function getSafeRedirectPath(redirect) {
      if (!redirect || typeof redirect !== 'string') return '/web/index.html';

      const candidate = redirect.trim();
      if (!candidate.startsWith('/') || candidate.startsWith('//')) {
        return '/web/index.html';
      }

      try {
        const url = new URL(candidate, window.location.origin);
        if (url.origin !== window.location.origin) return '/web/index.html';
        return `${url.pathname}${url.search}${url.hash}`;
      } catch (_) {
        return '/web/index.html';
      }
    }

    // 表单提交处理
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      hideError();
      setLoading(true);

      const password = passwordInput.value;

      try {
        const resp = await fetchAPI('/login', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
          },
          body: JSON.stringify({ password }),
        });

        if (resp.success) {
          const data = resp.data || {};

          // 存储Token到localStorage
          localStorage.setItem('ccload_token', data.token);
          localStorage.setItem('ccload_token_expiry', Date.now() + data.expiresIn * 1000);

          // 登录成功，添加成功动画
          loginButton.style.background = 'linear-gradient(135deg, var(--success-500), var(--success-600))';

          setTimeout(() => {
            const urlParams = new URLSearchParams(window.location.search);
            const redirect = getSafeRedirectPath(urlParams.get('redirect'));
            window.location.href = redirect;
          }, 500);
        } else {
          showError(resp.error || '密码错误，请重试');

          // 添加输入框摇晃动画
          passwordInput.style.animation = 'none';
          passwordInput.offsetHeight;
          passwordInput.style.animation = 'shake 0.5s ease-in-out';

          setTimeout(() => {
            passwordInput.style.animation = '';
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
