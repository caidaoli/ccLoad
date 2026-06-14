/**
 * 介绍网站通用交互逻辑
 * 功能：代码复制、Tab 切换、锚点平滑滚动
 */
(function() {
  'use strict';

  /**
   * 代码复制功能
   */
  function initCodeCopy() {
    document.querySelectorAll('.www-code-copy').forEach(button => {
      button.addEventListener('click', async () => {
        const codeBlock = button.closest('.www-code-block');
        const codeContent = codeBlock.querySelector('pre')?.textContent || '';

        try {
          await navigator.clipboard.writeText(codeContent);

          // 更新按钮状态
          const originalText = button.textContent;
          button.textContent = window.t ? window.t('www.common.copied') : '已复制';
          button.classList.add('copied');

          setTimeout(() => {
            button.textContent = originalText;
            button.classList.remove('copied');
          }, 2000);
        } catch (err) {
          console.error('Failed to copy code:', err);
          alert('复制失败，请手动选择复制');
        }
      });
    });
  }

  /**
   * Tab 切换功能
   */
  function initTabs() {
    document.querySelectorAll('.www-tabs').forEach(tabsContainer => {
      const buttons = tabsContainer.querySelectorAll('.www-tab-button');
      const panels = tabsContainer.querySelectorAll('.www-tab-panel');

      buttons.forEach((button, index) => {
        button.addEventListener('click', () => {
          // 移除所有激活状态
          buttons.forEach(btn => btn.classList.remove('active'));
          panels.forEach(panel => panel.classList.remove('active'));

          // 激活当前 tab
          button.classList.add('active');
          if (panels[index]) {
            panels[index].classList.add('active');
          }
        });
      });
    });
  }

  /**
   * 锚点平滑滚动
   */
  function initSmoothScroll() {
    document.querySelectorAll('a[href^="#"]').forEach(anchor => {
      anchor.addEventListener('click', function(e) {
        const targetId = this.getAttribute('href');
        if (targetId === '#') return;

        const targetElement = document.querySelector(targetId);
        if (targetElement) {
          e.preventDefault();
          const navHeight = document.querySelector('.www-nav')?.offsetHeight || 64;
          const targetPosition = targetElement.offsetTop - navHeight - 20;

          window.scrollTo({
            top: targetPosition,
            behavior: 'smooth'
          });
        }
      });
    });
  }

  /**
   * 页脚内容生成
   */
  function initFooter() {
    const footer = document.querySelector('.www-footer');
    if (!footer) {
      // 创建页脚
      const footerHTML = `
        <footer class="www-footer">
          <div class="www-footer-bottom">
            <p>
              © 2025 ccLoad ·
              <a href="https://github.com/caidaoli/ccLoad/blob/master/LICENSE" target="_blank" rel="noopener" style="color: inherit; text-decoration: underline;">
                MIT License
              </a>
            </p>
          </div>
        </footer>
      `;

      document.body.insertAdjacentHTML('beforeend', footerHTML);

      // 如果 i18n 已加载，翻译页脚
      if (window.translatePage) {
        window.translatePage();
      }
    }
  }

  /**
   * 初始化所有功能
   */
  function init() {
    initCodeCopy();
    initTabs();
    initSmoothScroll();
    initFooter();

    // 监听语言变化，重新初始化代码复制按钮文本
    window.addEventListener('localechange', () => {
      // 更新复制按钮文本
      document.querySelectorAll('.www-code-copy').forEach(button => {
        if (!button.classList.contains('copied')) {
          button.textContent = window.t ? window.t('www.common.copy') : '复制';
        }
      });
    });
  }

  // DOM 加载完成后初始化
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  // 导出全局函数供其他脚本使用
  window.WWW = {
    initCodeCopy,
    initTabs,
    initSmoothScroll
  };
})();
