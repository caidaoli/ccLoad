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

  // 区块内容进入视口时渐入；网格内卡片按序错开。不支持或偏好减少动效时保持静态
  function initReveal() {
    if (!('IntersectionObserver' in window)) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    const GRID = '.www-bento, .www-feature-grid, .www-deployment-grid, .www-doc-grid, .www-step-list';
    const targets = [];
    document.querySelectorAll('.www-section .www-container > *, .www-cta-inner').forEach(el => {
      if (el.matches(GRID)) {
        Array.from(el.children).forEach((child, i) => {
          child.style.transitionDelay = `${Math.min(i, 6) * 60}ms`;
          targets.push(child);
        });
      } else {
        targets.push(el);
      }
    });

    const observer = new IntersectionObserver(entries => {
      entries.forEach(entry => {
        if (!entry.isIntersecting) return;
        const el = entry.target;
        el.classList.add('is-visible');
        observer.unobserve(el);
        // 动画结束后撤掉临时类，恢复卡片自身的 hover 过渡
        setTimeout(() => {
          el.classList.remove('www-reveal', 'is-visible');
          el.style.transitionDelay = '';
        }, 1000);
      });
    }, { rootMargin: '0px 0px -8% 0px' });

    targets.forEach(el => {
      el.classList.add('www-reveal');
      observer.observe(el);
    });
  }

  function init() {
    initCodeCopy();
    initTabs();
    initSmoothScroll();
    initReveal();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
