(function () {
  function joinClasses(...classes) {
    return classes.filter(Boolean).join(' ');
  }

  function buildFilterGroup(content, extraClass = '') {
    return `<div class="${joinClasses('filter-group', extraClass)}">${content}</div>`;
  }

  function buildFilterLabel(forId, i18nKey, text) {
    return `<label for="${forId}" class="filter-label" data-i18n="${i18nKey}">${text}</label>`;
  }

  function buildSelect(id, optionsHtml = '', style = '') {
    const styleAttr = style ? ` style="${style}"` : '';
    return `<select id="${id}" class="filter-select"${styleAttr}>${optionsHtml}</select>`;
  }

  function buildInput(type, id, placeholderKey, placeholder, style = '') {
    const styleAttr = style ? ` style="${style}"` : '';
    return `<input type="${type}" id="${id}" class="filter-input" data-i18n-placeholder="${placeholderKey}" placeholder="${placeholder}"${styleAttr}>`;
  }

  function buildSharedFields(config) {
    const groupClass = config.groupClass || '';
    const infoClass = config.infoClass || 'filter-info';

    return {
      channelType: buildFilterGroup(
        `${buildFilterLabel('f_channel_type', 'stats.channelType', '渠道类型')}
        ${buildSelect('f_channel_type', '\n                <!-- 动态加载渠道类型选项 -->\n              ', 'min-width: 120px;')}`,
        groupClass
      ),
      timeRange: buildFilterGroup(
        `${buildFilterLabel('f_hours', 'stats.timeRange', '时间范围')}
        ${buildSelect('f_hours', '\n                <!-- 动态生成选项 by date-range-selector.js -->\n              ', 'min-width: 100px;')}`,
        groupClass
      ),
      channelId: buildFilterGroup(
        `${buildFilterLabel('f_id', 'stats.channelId', '渠道ID')}
        ${buildInput('number', 'f_id', 'stats.inputIdPlaceholder', '输入ID...', 'max-width: 100px;')}`,
        groupClass
      ),
      channelName: buildFilterGroup(
        `${buildFilterLabel('f_name', 'stats.channelName', '渠道名')}
        ${buildInput('text', 'f_name', 'stats.containsTextPlaceholder', '包含文本...')}`,
        groupClass
      ),
      modelText: buildFilterGroup(
        `${buildFilterLabel('f_model', 'common.model', '模型')}
        ${buildInput('text', 'f_model', 'stats.containsTextPlaceholder', '包含文本...')}`,
        groupClass
      ),
      modelSelect: buildFilterGroup(
        `${buildFilterLabel('f_model', 'common.model', '模型')}
        ${buildSelect('f_model', '\n                <option value="" data-i18n="trend.allModels">全部模型</option>\n                <!-- 动态加载模型列表 -->\n              ', 'min-width: 150px;')}`,
        groupClass
      ),
      authToken: buildFilterGroup(
        `${buildFilterLabel('f_auth_token', 'stats.token', '令牌')}
        ${buildSelect('f_auth_token', '\n                <option value="" data-i18n="stats.allTokens">全部令牌</option>\n                <!-- 动态加载令牌列表 -->\n              ', 'min-width: 150px;')}`,
        groupClass
      ),
      status: buildFilterGroup(
        `${buildFilterLabel('f_status', 'logs.statusCode', '状态码')}
        ${buildInput('number', 'f_status', 'logs.statusPlaceholder', '如 200 / 403', 'max-width: 110px;')}`,
        groupClass
      ),
      logsInfo: `<div class="${infoClass}"><span data-i18n="logs.showPrefix">显示</span><span id="displayedCount">0</span>/<span id="totalCount">0</span><span data-i18n="logs.recordsSuffix">条</span></div>`,
      statsInfo: `<div class="${infoClass}"><span data-i18n="stats.totalRecordsPrefix">共</span> <span id="statsCount">0</span> <span data-i18n="stats.totalRecordsSuffix">条记录</span></div>`,
      hideZeroSuccess: `<div class="filter-group" style="align-items: center;">
              <label class="filter-checkbox-label">
                <input type="checkbox" id="f_hide_zero_success" checked>
                <span data-i18n="stats.hideZeroSuccess">隐藏0成功</span>
              </label>
            </div>`,
      filterButton: config.actionsClass
        ? `<div class="${config.actionsClass}">
              <button id="btn_filter" type="button" class="btn btn-primary" style="padding: 8px 16px; font-size: 14px;" data-i18n="common.filter">筛选</button>
            </div>`
        : `<div style="flex: none;">
              <button id="btn_filter" type="button" class="btn btn-primary" style="padding: 8px 16px; font-size: 14px;" data-i18n="common.filter">筛选</button>
            </div>`
    };
  }

  const LAYOUTS = {
    stats: {
      barClass: 'filter-bar mt-2',
      controlsClass: 'filter-controls',
      groupClass: '',
      infoClass: 'filter-info',
      actionsClass: '',
      items: ['channelType', 'timeRange', 'channelId', 'channelName', 'modelText', 'authToken', 'hideZeroSuccess', 'statsInfo', 'filterButton']
    },
    logs: {
      barClass: 'filter-bar logs-filter-bar mt-2',
      controlsClass: 'filter-controls logs-filter-controls',
      groupClass: 'logs-filter-group',
      infoClass: 'filter-info logs-filter-info',
      actionsClass: 'logs-filter-actions',
      items: ['channelType', 'timeRange', 'channelId', 'channelName', 'modelText', 'status', 'authToken', 'logsInfo', 'filterButton']
    },
    trend: {
      barClass: 'filter-bar mt-2',
      controlsClass: 'filter-controls',
      groupClass: '',
      infoClass: 'filter-info',
      actionsClass: '',
      items: ['channelType', 'timeRange', 'channelId', 'channelName', 'modelSelect', 'authToken', 'filterButton']
    }
  };

  function renderLayout(layoutName) {
    const config = LAYOUTS[layoutName];
    if (!config) {
      console.error(`[PageFilters] Unknown layout: ${layoutName}`);
      return '';
    }

    const fields = buildSharedFields(config);
    const content = config.items
      .map((item) => fields[item] || '')
      .filter(Boolean)
      .join('\n');

    return `<div class="${config.barClass}">
          <div class="${config.controlsClass}">
            ${content}
          </div>
        </div>`;
  }

  function initPageFilters(root = document) {
    if (!root || typeof root.querySelectorAll !== 'function') return;

    root.querySelectorAll('[data-page-filters]').forEach((container) => {
      const layoutName = container.getAttribute('data-page-filters');
      if (!layoutName) return;
      container.innerHTML = renderLayout(layoutName);
    });
  }

  window.PageFilters = {
    renderLayout,
    initPageFilters
  };

  initPageFilters();
})();
