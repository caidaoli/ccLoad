/**
 * Shared interactions for the website
 * Features: code copy, accessible tabs, reveal-on-scroll and doc TOC highlighting. Text is pre-rendered per language by build.mjs.
 * Anchor scrolling is native: html sets scroll-padding-top, and smooth scrolling only when reduced motion is not requested.
 */
(function() {
  'use strict';

  const zh = document.documentElement.lang === 'zh-CN';
  const COPIED_TEXT = zh ? '已复制！' : 'Copied!';
  const COPY_FAILED_TEXT = zh ? '复制失败' : 'Copy failed';

  // 结果直接显示在按钮上（不弹 alert），2 秒后恢复；连续点击只延长显示，不会把提示当成原文
  function initCodeCopy() {
    document.querySelectorAll('.www-code-copy').forEach(button => {
      const label = button.textContent;
      let timer;
      button.addEventListener('click', async () => {
        const code = button.closest('.www-code-block').querySelector('pre')?.textContent || '';
        let copied = true;
        try {
          await navigator.clipboard.writeText(code);
        } catch (err) {
          console.error('Failed to copy code:', err);
          copied = false;
        }
        button.textContent = copied ? COPIED_TEXT : COPY_FAILED_TEXT;
        button.classList.toggle('copied', copied);
        clearTimeout(timer);
        timer = setTimeout(() => {
          button.textContent = label;
          button.classList.remove('copied');
        }, 2000);
      });
    });
  }

  // WAI-ARIA tabs：tablist/tab/tabpanel 关联，左右方向键与 Home/End 切换，roving tabindex 让 Tab 键只停在当前页签
  function initTabs() {
    document.querySelectorAll('.www-tabs').forEach((tabs, group) => {
      const buttons = Array.from(tabs.querySelectorAll('.www-tab-button'));
      const panels = Array.from(tabs.querySelectorAll('.www-tab-panel'));
      tabs.querySelector('.www-tab-list')?.setAttribute('role', 'tablist');

      const select = (index, focus) => {
        buttons.forEach((button, i) => {
          const active = i === index;
          button.classList.toggle('active', active);
          button.setAttribute('aria-selected', String(active));
          button.tabIndex = active ? 0 : -1;
          panels[i]?.classList.toggle('active', active);
        });
        if (focus) buttons[index].focus();
      };

      buttons.forEach((button, i) => {
        button.type = 'button';
        button.id = `www-tab-${group}-${i}`;
        button.setAttribute('role', 'tab');
        if (panels[i]) {
          panels[i].id = `${button.id}-panel`;
          panels[i].setAttribute('role', 'tabpanel');
          panels[i].setAttribute('aria-labelledby', button.id);
          button.setAttribute('aria-controls', panels[i].id);
        }
        button.addEventListener('click', () => select(i, false));
        button.addEventListener('keydown', event => {
          const last = buttons.length - 1;
          const next = { ArrowRight: i === last ? 0 : i + 1, ArrowLeft: i === 0 ? last : i - 1, Home: 0, End: last }[event.key];
          if (next === undefined) return;
          event.preventDefault();
          select(next, true);
        });
      });
      select(Math.max(0, buttons.findIndex(button => button.classList.contains('active'))), false);
    });
  }

  // 区块内容进入视口时渐入；网格内卡片按序错开。首屏已可见的内容不参与，避免加载后先消失再出现；
  // 不支持或偏好减少动效时保持静态
  function initReveal() {
    if (!('IntersectionObserver' in window)) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    const GRID = '.www-bento, .www-feature-grid, .www-doc-grid, .www-step-list';
    const targets = [];
    document.querySelectorAll('.www-section .www-container > *, .www-cta-inner').forEach(el => {
      if (el.matches(GRID)) {
        Array.from(el.children).forEach((child, i) => targets.push([child, `${Math.min(i, 6) * 60}ms`]));
      } else {
        targets.push([el, '']);
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

    // 先统一读取位置再写样式，避免逐个强制重排
    targets
      .filter(([el]) => el.getBoundingClientRect().top >= window.innerHeight)
      .forEach(([el, delay]) => {
        el.style.transitionDelay = delay;
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
    initReveal();
    initTocSpy();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
