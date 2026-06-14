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
          <div class="www-footer-content">
            <div class="www-footer-section">
              <h4 class="www-footer-title" data-i18n="www.footer.resources">资源</h4>
              <a href="https://github.com/caidaoli/ccLoad" class="www-footer-link" target="_blank" rel="noopener">
                GitHub
              </a>
              <a href="https://github.com/caidaoli/ccLoad/releases" class="www-footer-link" target="_blank" rel="noopener" data-i18n="www.footer.releases">
                版本发布
              </a>
              <a href="https://github.com/caidaoli/ccLoad/blob/master/README.md" class="www-footer-link" target="_blank" rel="noopener" data-i18n="www.footer.documentation">
                文档
              </a>
            </div>

            <div class="www-footer-section">
              <h4 class="www-footer-title" data-i18n="www.footer.community">社区</h4>
              <a href="https://github.com/caidaoli/ccLoad/issues" class="www-footer-link" target="_blank" rel="noopener">
                Issues
              </a>
              <a href="https://github.com/caidaoli/ccLoad/discussions" class="www-footer-link" target="_blank" rel="noopener">
                Discussions
              </a>
              <a href="https://github.com/caidaoli/ccLoad/pulls" class="www-footer-link" target="_blank" rel="noopener">
                Pull Requests
              </a>
            </div>

            <div class="www-footer-section">
              <h4 class="www-footer-title" data-i18n="www.footer.links">链接</h4>
              <a href="/web/index.html" class="www-footer-link" data-i18n="www.footer.adminPanel">
                管理后台
              </a>
              <a href="https://www.anthropic.com" class="www-footer-link" target="_blank" rel="noopener">
                Anthropic
              </a>
              <a href="https://github.com/caidaoli" class="www-footer-link" target="_blank" rel="noopener">
                @caidaoli
              </a>
            </div>
          </div>

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
