/**
 * Shared interactions for the website
 * Features: code copy, tab switching, and smooth anchor scrolling. Text is pre-rendered per language by build.mjs.
 */
(function() {
  'use strict';

  const zh = document.documentElement.lang === 'zh-CN';
  const COPIED_TEXT = zh ? '已复制！' : 'Copied!';
  const COPY_FAILED_TEXT = zh ? '复制失败，请手动选择复制' : 'Copy failed. Please select the text and copy it manually.';

  function initCodeCopy() {
    document.querySelectorAll('.www-code-copy').forEach(button => {
      button.addEventListener('click', async () => {
        const codeBlock = button.closest('.www-code-block');
        const codeContent = codeBlock.querySelector('pre')?.textContent || '';

        try {
          await navigator.clipboard.writeText(codeContent);
          const originalText = button.textContent;
          button.textContent = COPIED_TEXT;
          button.classList.add('copied');

          setTimeout(() => {
            button.textContent = originalText;
            button.classList.remove('copied');
          }, 2000);
        } catch (err) {
          console.error('Failed to copy code:', err);
          alert(COPY_FAILED_TEXT);
        }
      });
    });
  }

  function initTabs() {
    document.querySelectorAll('.www-tabs').forEach(tabsContainer => {
      const buttons = tabsContainer.querySelectorAll('.www-tab-button');
      const panels = tabsContainer.querySelectorAll('.www-tab-panel');

      buttons.forEach((button, index) => {
        button.addEventListener('click', () => {
          buttons.forEach(btn => btn.classList.remove('active'));
          panels.forEach(panel => panel.classList.remove('active'));
          button.classList.add('active');
          if (panels[index]) {
            panels[index].classList.add('active');
          }
        });
      });
    });
  }

  function initSmoothScroll() {
    document.querySelectorAll('a[href^="#"]').forEach(anchor => {
      anchor.addEventListener('click', function(e) {
        const targetId = this.getAttribute('href');
        if (targetId === '#') return;

        const targetElement = document.querySelector(targetId);
        if (targetElement) {
          e.preventDefault();
          const navHeight = document.querySelector('.www-nav')?.offsetHeight || 64;
          window.scrollTo({
            top: targetElement.offsetTop - navHeight - 20,
            behavior: 'smooth'
          });
          history.replaceState(null, '', targetId);
        }
      });
    });
  }

  function init() {
    initCodeCopy();
    initTabs();
    initSmoothScroll();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
