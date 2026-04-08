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

  function buildSelect(id, optionsHtml = '', extraClass = '') {
    return `<select id="${id}" class="${joinClasses('filter-select', extraClass)}">${optionsHtml}</select>`;
  }

  function buildInput(type, id, placeholderKey, placeholder, extraClass = '') {
    return `<input type="${type}" id="${id}" class="${joinClasses('filter-input', extraClass)}" data-i18n-placeholder="${placeholderKey}" placeholder="${placeholder}">`;
  }

  function buildSharedFields(config) {
    const groupClass = config.groupClass || '';
    const infoClass = config.infoClass || 'filter-info';
    const checkboxGroupClass = config.checkboxGroupClass || groupClass;
    const hideZeroSuccess = `<div class="${joinClasses('filter-group', 'filter-group--checkbox', checkboxGroupClass)}">
              <label class="filter-checkbox-label">
                <input type="checkbox" id="f_hide_zero_success" checked>
                <span data-i18n="stats.hideZeroSuccess">隐藏0成功</span>
              </label>
            </div>`;
    const statsInfo = `<div class="${infoClass}"><span data-i18n="stats.totalRecordsPrefix">共</span> <span id="statsCount">0</span> <span data-i18n="stats.totalRecordsSuffix">条记录</span></div>`;
    const filterButton = `<div class="${joinClasses('filter-actions', 'filter-actions--page', config.actionsClass)}">
              <button id="btn_filter" type="button" class="btn btn-primary filter-btn" data-i18n="common.filter">筛选</button>
            </div>`;
    const logsInfo = `<div class="${infoClass}"><span data-i18n="logs.showPrefix">显示</span><span id="displayedCount">0</span>/<span id="totalCount">0</span><span data-i18n="logs.recordsSuffix">条</span></div>`;

    return {
      channelType: buildFilterGroup(
        `${buildFilterLabel('f_channel_type', 'stats.channelType', '渠道类型')}
        ${buildSelect('f_channel_type', '\n                <!-- 动态加载渠道类型选项 -->\n              ', 'filter-control--compact')}`,
        groupClass
      ),
      timeRange: buildFilterGroup(
        `${buildFilterLabel('f_hours', 'stats.timeRange', '时间范围')}
        ${buildSelect('f_hours', '\n                <!-- 动态生成选项 by date-range-selector.js -->\n              ', 'filter-control--compact')}`,
        groupClass
      ),
      channelId: buildFilterGroup(
        `${buildFilterLabel('f_id', 'stats.channelId', '渠道ID')}
        ${buildInput('number', 'f_id', 'stats.inputIdPlaceholder', '输入ID...', 'filter-control--narrow')}`,
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
        ${buildSelect('f_model', '\n                <option value="" data-i18n="trend.allModels">全部模型</option>\n                <!-- 动态加载模型列表 -->\n              ', 'filter-control--wide')}`,
        groupClass
      ),
      authToken: buildFilterGroup(
        `${buildFilterLabel('f_auth_token', 'stats.token', '令牌')}
        ${buildSelect('f_auth_token', '\n                <option value="" data-i18n="stats.allTokens">全部令牌</option>\n                <!-- 动态加载令牌列表 -->\n              ', 'filter-control--wide')}`,
        groupClass
      ),
      status: buildFilterGroup(
        `${buildFilterLabel('f_status', 'logs.statusCode', '状态码')}
        ${buildInput('number', 'f_status', 'logs.statusPlaceholder', '如 200 / 403', 'filter-control--narrow')}`,
        groupClass
      ),
      logSource: buildFilterGroup(
        `${buildFilterLabel('f_log_source', 'logs.logSource', '日志来源')}
        ${buildSelect('f_log_source', `
                <option value="proxy" data-i18n="logs.sourceProxy">请求日志</option>
                <option value="detection" data-i18n="logs.sourceDetection">检测日志</option>
                <option value="all" data-i18n="logs.sourceAll">全部日志</option>
              `, 'filter-control--compact')}`,
        groupClass
      ),
      logsInfo,
      statsInfo,
      hideZeroSuccess,
      filterButton,
      logsSummary: `<div class="logs-filter-summary-row">${logsInfo}${filterButton}</div>`,
      statsSummary: `<div class="stats-filter-summary-row">${hideZeroSuccess}${statsInfo}${filterButton}</div>`
    };
  }

  const LAYOUTS = {
    stats: {
      barClass: 'filter-bar stats-filter-bar mt-2',
      controlsClass: 'filter-controls stats-filter-controls',
      groupClass: 'stats-filter-group',
      checkboxGroupClass: 'stats-filter-group stats-filter-group--checkbox',
      infoClass: 'filter-info stats-filter-info',
      actionsClass: 'stats-filter-actions',
      items: ['channelType', 'timeRange', 'channelId', 'channelName', 'modelText', 'authToken', 'statsSummary']
    },
    logs: {
      barClass: 'filter-bar logs-filter-bar mt-2',
      controlsClass: 'filter-controls logs-filter-controls',
      groupClass: 'logs-filter-group',
      infoClass: 'filter-info logs-filter-info',
      actionsClass: 'logs-filter-actions',
      items: ['channelType', 'timeRange', 'channelId', 'channelName', 'modelText', 'logSource', 'status', 'authToken', 'logsSummary']
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
