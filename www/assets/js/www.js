/**
 * Shared interactions for the website
 * Features: code copy, tab switching, smooth anchor scrolling and doc TOC highlighting. Text is pre-rendered per language by build.mjs.
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
          // 顶部留白由 html 的 scroll-padding-top 统一给出
          targetElement.scrollIntoView({ behavior: 'smooth' });
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

  // 文档页左侧导航高亮当前阅读的区块：取顶部已越过视口 30% 线的最后一节；滚到底时高亮最后一节
  function initTocSpy() {
    const pairs = Array.from(document.querySelectorAll('.www-doc-toc a[href^="#"]'))
      .map(link => [link, document.getElementById(link.getAttribute('href').slice(1))])
      .filter(([, section]) => section);
    if (!pairs.length) return;

    let pending = false;
    const update = () => {
      pending = false;
      const line = window.innerHeight * 0.3;
      let current = 0;
      pairs.forEach(([, section], i) => {
        if (section.getBoundingClientRect().top <= line) current = i;
      });
      if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2) {
        current = pairs.length - 1;
      }
      pairs.forEach(([link], i) => link.classList.toggle('active', i === current));
    };
    window.addEventListener('scroll', () => {
      if (pending) return;
      pending = true;
      requestAnimationFrame(update);
    }, { passive: true });
    update();
  }

  function init() {
    initCodeCopy();
    initTabs();
    initSmoothScroll();
    initReveal();
    initTocSpy();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
