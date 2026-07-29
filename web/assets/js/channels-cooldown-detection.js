(function () {
  'use strict';

  const MAX_RULES = 32;
  const MAX_RULE_NAME_LENGTH = 128;
  const MAX_PATTERN_LENGTH = 1024;
  const MAX_TIME_FIELD_LENGTH = 128;
  const MAX_COOLDOWN_SECONDS = 366 * 24 * 60 * 60;
  const VALID_SCOPES = new Set(['key', 'model', 'channel']);
  const VALID_MODES = new Set(['fixed', 'reset_time']);
  const DEFAULT_TIME_FORMAT = 'datetime';
  const TIME_FORMAT_TIME_OF_DAY = 'time_of_day';
  const VALID_TIME_FORMATS = new Set(['datetime', TIME_FORMAT_TIME_OF_DAY, 'unix', 'unix_ms', 'duration_seconds']);
  const hasWindow = typeof window !== 'undefined';
  const hasDocument = typeof document !== 'undefined';

  let _state = { rules: [] };
  let _draft = null;

  function t(key, fallback) {
    if (hasWindow && typeof window.t === 'function') {
      const value = window.t(key);
      if (value && value !== key) return value;
    }
    return fallback;
  }

  function text(value) {
    return value == null ? '' : String(value);
  }

  function normalizedPriority(value, fallback) {
    const parsed = Number(value);
    return Number.isInteger(parsed) && parsed >= 0 ? parsed : fallback;
  }

  function statusCodesText(value) {
    if (Array.isArray(value)) return value.map((item) => text(item).trim()).filter(Boolean).join(', ');
    return text(value).trim();
  }

  function cloneRules(source) {
    const safe = source && typeof source === 'object' ? source : {};
    const rawRules = Array.isArray(safe.rules) ? safe.rules : [];
    const rules = rawRules.map((raw, index) => {
      const rule = raw && typeof raw === 'object' ? raw : {};
      return {
        enabled: rule.enabled !== false,
        name: text(rule.name),
        priority: normalizedPriority(rule.priority, index),
        status_codes: statusCodesText(rule.status_codes),
        message_pattern: text(rule.message_pattern),
        scope: text(rule.scope || 'key').trim().toLowerCase(),
        mode: text(rule.mode || 'fixed').trim().toLowerCase(),
        cooldown_seconds: rule.cooldown_seconds == null ? '60' : text(rule.cooldown_seconds),
        time_capture: text(rule.time_capture),
        time_format: text(rule.time_format || DEFAULT_TIME_FORMAT).trim().toLowerCase(),
        time_layout: text(rule.time_layout),
        timezone: text(rule.timezone)
      };
    });
    rules.sort((left, right) => left.priority - right.priority);
    rules.forEach((rule, index) => { rule.priority = index; });
    return { rules };
  }

  function getState() {
    if (!_state || typeof _state !== 'object') _state = { rules: [] };
    if (!Array.isArray(_state.rules)) _state.rules = [];
    return _state;
  }

  function resetCooldownDetectionState(rules) {
    _state = cloneRules(rules);
    if (hasWindow) window.channelCooldownDetectionState = _state;
    updateRuleCount(_state);
  }

  function updateRuleCount(source) {
    if (!hasDocument) return;
    const count = Array.isArray(source && source.rules) ? source.rules.length : 0;
    const element = document.getElementById('cooldownDetectionRulesCount');
    if (element) element.textContent = `(${count})`;
  }

  function parseStatusCodes(value) {
    const parts = text(value).split(',').map((part) => part.trim()).filter(Boolean);
    const codes = [];
    const errors = [];
    const seen = new Set();
    for (const part of parts) {
      if (!/^\d+$/.test(part)) {
        errors.push(t('channels.cooldownDetection.errStatusCode', `Invalid status code: ${part}`));
        continue;
      }
      const code = Number(part);
      if (!Number.isInteger(code) || code < 100 || code > 599) {
        errors.push(t('channels.cooldownDetection.errStatusRange', `Status code must be between 100 and 599: ${part}`));
        continue;
      }
      if (seen.has(code)) {
        errors.push(t('channels.cooldownDetection.errDuplicateStatus', `Duplicate status code: ${code}`));
        continue;
      }
      seen.add(code);
      codes.push(code);
    }
    return { codes, errors };
  }

  function validateCooldownDetectionRulesLocally(source) {
    const rules = cloneRules(source).rules;
    const errors = [];
    if (rules.length > MAX_RULES) {
      errors.push(t('channels.cooldownDetection.errMaxRules', `Too many rules (max ${MAX_RULES})`));
    }
    rules.forEach((rule, index) => {
      const label = `${t('channels.cooldownDetection.rule', 'Rule')} #${index + 1}`;
      if (!rule.name.trim()) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errNameRequired', 'Rule name is required')}`);
      } else if (rule.name.trim().length > MAX_RULE_NAME_LENGTH) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errNameLength', `Rule name cannot exceed ${MAX_RULE_NAME_LENGTH} characters`)}`);
      }
      const status = parseStatusCodes(rule.status_codes);
      status.errors.forEach((message) => errors.push(`${label}: ${message}`));
      const messagePattern = rule.message_pattern.trim();
      if (status.codes.length === 0 && !messagePattern) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errCondition', 'At least one match condition is required')}`);
      }
      if (messagePattern.length > MAX_PATTERN_LENGTH) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errPatternLength', `Regex cannot exceed ${MAX_PATTERN_LENGTH} characters`)}`);
      }
      if (!VALID_SCOPES.has(rule.scope)) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errScope', 'Invalid cooldown scope')}`);
      }
      if (!VALID_MODES.has(rule.mode)) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errMode', 'Invalid cooldown mode')}`);
        return;
      }
      if (rule.mode === 'fixed') {
        const seconds = Number(rule.cooldown_seconds);
        if (!Number.isInteger(seconds) || seconds <= 0 || seconds > MAX_COOLDOWN_SECONDS) {
          errors.push(`${label}: ${t('channels.cooldownDetection.errDuration', 'Fixed cooldown must be a positive duration within one year')}`);
        }
        return;
      }
      if (!messagePattern) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errDynamicMessage', 'A message regex is required for dynamic reset time')}`);
      }
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(rule.time_capture.trim())) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errCapture', 'A named capture is required')}`);
      }
      if (!VALID_TIME_FORMATS.has(rule.time_format)) {
        errors.push(`${label}: ${t('channels.cooldownDetection.errTimeFormat', 'Invalid time value type')}`);
        return;
      }
      if (usesTimeLayout(rule.time_format)) {
        if (!rule.time_layout.trim() || rule.time_layout.trim().length > MAX_TIME_FIELD_LENGTH) {
          errors.push(`${label}: ${t('channels.cooldownDetection.errTimeLayout', 'A valid Go time layout is required')}`);
        }
        if (!rule.timezone.trim() || rule.timezone.trim().length > MAX_TIME_FIELD_LENGTH) {
          errors.push(`${label}: ${t('channels.cooldownDetection.errTimezone', 'An IANA timezone is required')}`);
        }
      }
    });
    return errors;
  }

  function serializeRules(source) {
    return cloneRules(source).rules.map((rule, index) => {
      const status = parseStatusCodes(rule.status_codes);
      const entry = {
        enabled: Boolean(rule.enabled),
        priority: index,
        scope: rule.scope,
        mode: rule.mode
      };
      if (rule.name.trim()) entry.name = rule.name.trim();
      if (status.codes.length > 0) entry.status_codes = status.codes;
      if (rule.message_pattern.trim()) entry.message_pattern = rule.message_pattern.trim();
      if (rule.mode === 'fixed') {
        entry.cooldown_seconds = Number(rule.cooldown_seconds);
      } else {
        entry.time_capture = rule.time_capture.trim();
        entry.time_format = rule.time_format;
        if (usesTimeLayout(rule.time_format)) {
          entry.time_layout = rule.time_layout.trim();
          entry.timezone = rule.timezone.trim();
        }
      }
      return entry;
    });
  }

  function collectCooldownDetectionRulesForSubmit() {
    const state = getState();
    if (state.rules.length === 0) return null;
    return { rules: serializeRules(state) };
  }

  function validateCooldownDetectionRulesForSubmit() {
    return validateCooldownDetectionRulesLocally(getState());
  }

  function beginCooldownDetectionDraft() {
    if (!hasDocument) return;
    if (!_draft) {
      _draft = cloneRules(getState());
      clearTestResult();
    }
    renderRuleList();
    hideError();
  }

  function discardCooldownDetectionDraft() {
    if (!hasDocument) return;
    _draft = null;
    hideError();
  }

  function addCooldownDetectionRule() {
    if (!_draft) return;
    if (_draft.rules.length >= MAX_RULES) {
      showError(t('channels.cooldownDetection.errMaxRules', `Too many rules (max ${MAX_RULES})`));
      return;
    }
    const ordinal = _draft.rules.length + 1;
    _draft.rules.push({
      enabled: true,
      name: t('channels.cooldownDetection.defaultName', 'Cooldown rule {number}').replace('{number}', String(ordinal)),
      priority: _draft.rules.length,
      status_codes: '200',
      message_pattern: '',
      scope: 'key',
      mode: 'fixed',
      cooldown_seconds: '60',
      time_capture: 'reset_time',
      time_format: DEFAULT_TIME_FORMAT,
      time_layout: '2006-01-02 15:04:05',
      timezone: 'UTC'
    });
    renderRuleList();
    hideError();
  }

  function removeCooldownDetectionRule(index) {
    if (!_draft || !Number.isInteger(index) || index < 0 || index >= _draft.rules.length) return;
    _draft.rules.splice(index, 1);
    normalizeDraftPriorities();
    renderRuleList();
  }

  function moveCooldownDetectionRule(index, direction) {
    if (!_draft || !Number.isInteger(index) || !Number.isInteger(direction)) return;
    const target = index + direction;
    if (index < 0 || target < 0 || index >= _draft.rules.length || target >= _draft.rules.length) return;
    const [rule] = _draft.rules.splice(index, 1);
    _draft.rules.splice(target, 0, rule);
    normalizeDraftPriorities();
    renderRuleList();
  }

  function setCooldownDetectionRulePriority(index, oneBasedPriority) {
    if (!_draft || !Number.isInteger(index)) return;
    const requested = Number(oneBasedPriority);
    if (!Number.isInteger(requested)) return;
    const target = Math.max(0, Math.min(_draft.rules.length - 1, requested - 1));
    if (index < 0 || index >= _draft.rules.length || target === index) return;
    const [rule] = _draft.rules.splice(index, 1);
    _draft.rules.splice(target, 0, rule);
    normalizeDraftPriorities();
    renderRuleList();
  }

  function normalizeDraftPriorities() {
    if (!_draft || !Array.isArray(_draft.rules)) return;
    _draft.rules.forEach((rule, index) => { rule.priority = index; });
  }

  function renderRuleList() {
    if (!hasDocument || !_draft) return;
    const list = document.getElementById('cooldownDetectionRulesList');
    if (!list) return;
    list.innerHTML = '';
    if (_draft.rules.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'cooldown-detection-empty';
      empty.textContent = t('channels.cooldownDetection.empty', 'No cooldown detection rules yet.');
      list.appendChild(empty);
      return;
    }
    _draft.rules.forEach((rule, index) => list.appendChild(buildRuleCard(rule, index)));
  }

  function buildRuleCard(rule, index) {
    const card = document.createElement('section');
    card.className = 'cooldown-detection-rule';

    const header = document.createElement('div');
    header.className = 'cooldown-detection-rule-header';
    const enabledLabel = document.createElement('label');
    enabledLabel.className = 'cooldown-detection-enabled';
    const enabled = document.createElement('input');
    enabled.type = 'checkbox';
    enabled.checked = Boolean(rule.enabled);
    enabled.title = t('channels.cooldownDetection.enabledHelp', 'Disabled rules are skipped during matching.');
    enabled.addEventListener('change', () => { rule.enabled = enabled.checked; });
    enabledLabel.append(enabled, document.createTextNode(` ${t('channels.cooldownDetection.enabled', 'Enabled')}`));
    header.appendChild(enabledLabel);

    const name = document.createElement('label');
    name.className = 'cooldown-detection-name';
    const nameLabel = document.createElement('span');
    nameLabel.textContent = t('channels.cooldownDetection.name', 'Rule name');
    const nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.className = 'form-input';
    nameInput.maxLength = MAX_RULE_NAME_LENGTH;
    nameInput.value = text(rule.name);
    nameInput.placeholder = t('channels.cooldownDetection.nameHint', 'e.g. Provider rate limit');
    nameInput.setAttribute('aria-required', 'true');
    nameInput.title = t('channels.cooldownDetection.nameHelp', 'A required label for identifying this rule; it does not affect matching.');
    nameInput.addEventListener('input', () => { rule.name = nameInput.value; });
    name.append(nameLabel, nameInput);
    header.appendChild(name);

    const priority = document.createElement('label');
    priority.className = 'cooldown-detection-priority';
    const priorityLabel = document.createElement('span');
    priorityLabel.textContent = t('channels.cooldownDetection.priority', 'Priority');
    const priorityInput = document.createElement('input');
    priorityInput.type = 'number';
    priorityInput.className = 'form-input cooldown-detection-priority-input';
    priorityInput.min = '1';
    priorityInput.max = String(_draft.rules.length);
    priorityInput.step = '1';
    priorityInput.value = String(index + 1);
    priorityInput.title = t('channels.cooldownDetection.priorityHelp', 'Smaller numbers run first; matching stops at the first rule.');
    priorityInput.addEventListener('change', () => setCooldownDetectionRulePriority(index, priorityInput.value));
    priority.append(priorityLabel, priorityInput);
    header.appendChild(priority);

    const controls = document.createElement('div');
    controls.className = 'cooldown-detection-rule-controls';
    controls.appendChild(buildRuleActionButton('↑', 'move-cooldown-detection-rule', index, -1, index === 0, t('channels.cooldownDetection.moveUp', 'Move up')));
    controls.appendChild(buildRuleActionButton('↓', 'move-cooldown-detection-rule', index, 1, index === _draft.rules.length - 1, t('channels.cooldownDetection.moveDown', 'Move down')));
    controls.appendChild(buildRuleActionButton('×', 'remove-cooldown-detection-rule', index, null, false, t('channels.cooldownDetection.deleteRule', 'Delete rule')));
    header.appendChild(controls);
    card.appendChild(header);

    const conditions = document.createElement('div');
    conditions.className = 'cooldown-detection-grid cooldown-detection-grid--conditions';
    conditions.appendChild(buildTextField(t('channels.cooldownDetection.statusCodes', 'Status codes'), rule.status_codes, t('channels.cooldownDetection.statusCodesHint', '200, 429; empty = any'), (value) => { rule.status_codes = value; }, t('channels.cooldownDetection.statusCodesHelp', 'Comma-separated upstream HTTP status codes; leave blank to match any status.')));
    const messagePattern = buildTextField(t('channels.cooldownDetection.messagePattern', 'Message regex'), rule.message_pattern, t('channels.cooldownDetection.messagePatternHint', 'Supports Go named captures'), (value) => { rule.message_pattern = value; }, t('channels.cooldownDetection.messagePatternHelp', 'Go regular expression for the upstream error message. Reset-time mode requires a named capture.'));
    messagePattern.classList.add('cooldown-detection-message-pattern');
    conditions.appendChild(messagePattern);
    card.appendChild(conditions);

    const action = document.createElement('div');
    action.className = 'cooldown-detection-grid cooldown-detection-grid--action';
    action.appendChild(buildSelectField(t('channels.cooldownDetection.scope', 'Cooldown scope'), rule.scope, [
      ['key', t('channels.cooldownDetection.scopeKey', 'Current key')],
      ['model', t('channels.cooldownDetection.scopeModel', 'Actual model')],
      ['channel', t('channels.cooldownDetection.scopeChannel', 'Entire channel')]
    ], (value) => { rule.scope = value; }, t('channels.cooldownDetection.scopeHelp', 'Choose whether cooldown applies to the current key, the actual upstream model, or the entire channel.')));
    action.appendChild(buildSelectField(t('channels.cooldownDetection.mode', 'Cooldown mode'), rule.mode, [
      ['fixed', t('channels.cooldownDetection.modeFixed', 'Fixed duration')],
      ['reset_time', t('channels.cooldownDetection.modeResetTime', 'Parse reset time')]
    ], (value) => {
      rule.mode = value;
      updateModeFields(card, rule);
    }, t('channels.cooldownDetection.modeHelp', 'Fixed duration uses the configured seconds; reset time reads an exact end time from the error message.')));
    const fixed = buildNumberField(t('channels.cooldownDetection.duration', 'Duration (seconds)'), rule.cooldown_seconds, 1, MAX_COOLDOWN_SECONDS, (value) => { rule.cooldown_seconds = value; }, t('channels.cooldownDetection.durationHelp', 'Cooldown duration in seconds when using fixed duration mode.'));
    fixed.classList.add('cooldown-detection-fixed');
    action.appendChild(fixed);
    card.appendChild(action);

    const dynamic = document.createElement('div');
    dynamic.className = 'cooldown-detection-grid cooldown-detection-dynamic';
    dynamic.appendChild(buildTextField(t('channels.cooldownDetection.timeCapture', 'Time capture'), rule.time_capture, 'reset_time', (value) => { rule.time_capture = value; }, t('channels.cooldownDetection.timeCaptureHelp', 'The named regex capture containing the reset time, for example reset_time.')));
    const timeFormat = buildSelectField(t('channels.cooldownDetection.timeFormat', 'Time value type'), rule.time_format, [
      ['datetime', t('channels.cooldownDetection.timeFormatDateTime', 'Date and time')],
      [TIME_FORMAT_TIME_OF_DAY, t('channels.cooldownDetection.timeFormatTimeOfDay', 'Daily time')],
      ['unix', t('channels.cooldownDetection.timeFormatUnix', 'Unix seconds')],
      ['unix_ms', t('channels.cooldownDetection.timeFormatUnixMilliseconds', 'Unix milliseconds')],
      ['duration_seconds', t('channels.cooldownDetection.timeFormatDurationSeconds', 'Seconds from now')]
    ], (value) => {
      rule.time_format = value;
      updateTimeFormatFields(card, rule);
    }, t('channels.cooldownDetection.timeFormatHelp', 'Choose how to parse the captured value. Date and time and daily time require a layout and timezone.'));
    timeFormat.classList.add('cooldown-detection-time-format');
    dynamic.appendChild(timeFormat);
    const timeLayout = buildTextField(t('channels.cooldownDetection.timeLayout', 'Layout'), rule.time_layout, '2006-01-02 15:04:05', (value) => { rule.time_layout = value; }, t('channels.cooldownDetection.timeLayoutHelp', 'Go time layout for a date-and-time capture, for example 2006-01-02 15:04:05.'));
    timeLayout.classList.add('cooldown-detection-time-format-datetime');
    dynamic.appendChild(timeLayout);
    const timezone = buildTextField(t('channels.cooldownDetection.timezone', 'Timezone'), rule.timezone, 'UTC', (value) => { rule.timezone = value; }, t('channels.cooldownDetection.timezoneHelp', 'IANA timezone used when a captured time has no timezone, for example UTC or Asia/Shanghai.'));
    timezone.classList.add('cooldown-detection-time-format-datetime');
    dynamic.appendChild(timezone);
    card.appendChild(dynamic);

    updateModeFields(card, rule);
    return card;
  }

  function buildRuleActionButton(label, action, index, direction, disabled, title) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'btn btn-secondary cooldown-detection-icon-button';
    button.dataset.action = action;
    button.dataset.cooldownDetectionIndex = String(index);
    if (direction !== null) button.dataset.cooldownDetectionDirection = String(direction);
    button.disabled = disabled;
    button.textContent = label;
    button.title = title;
    button.setAttribute('aria-label', title);
    return button;
  }

  function buildTextField(labelText, value, placeholder, onInput, helpText) {
    const field = document.createElement('label');
    field.className = 'cooldown-detection-field';
    field.title = helpText || '';
    const label = document.createElement('span');
    label.textContent = labelText;
    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'form-input';
    input.value = text(value);
    input.placeholder = placeholder;
    input.title = helpText || '';
    input.addEventListener('input', () => onInput(input.value));
    field.append(label, input);
    return field;
  }

  function buildNumberField(labelText, value, min, max, onInput, helpText) {
    const field = document.createElement('label');
    field.className = 'cooldown-detection-field';
    field.title = helpText || '';
    const label = document.createElement('span');
    label.textContent = labelText;
    const input = document.createElement('input');
    input.type = 'number';
    input.className = 'form-input';
    input.min = String(min);
    input.max = String(max);
    input.step = '1';
    input.value = text(value);
    input.title = helpText || '';
    input.addEventListener('input', () => onInput(input.value));
    field.append(label, input);
    return field;
  }

  function buildSelectField(labelText, value, options, onChange, helpText) {
    const field = document.createElement('label');
    field.className = 'cooldown-detection-field';
    field.title = helpText || '';
    const label = document.createElement('span');
    label.textContent = labelText;
    const select = document.createElement('select');
    select.className = 'form-input';
    select.title = helpText || '';
    options.forEach(([optionValue, optionLabel]) => {
      const option = document.createElement('option');
      option.value = optionValue;
      option.textContent = optionLabel;
      option.selected = optionValue === value;
      select.appendChild(option);
    });
    select.addEventListener('change', () => onChange(select.value));
    field.append(label, select);
    return field;
  }

  function updateModeFields(card, rule) {
    const fixed = card.querySelector('.cooldown-detection-fixed');
    const dynamic = card.querySelector('.cooldown-detection-dynamic');
    const isDynamic = rule.mode === 'reset_time';
    if (fixed) fixed.hidden = isDynamic;
    if (dynamic) dynamic.hidden = !isDynamic;
    updateTimeFormatFields(card, rule);
  }

  function updateTimeFormatFields(card, rule) {
    const usesLayout = usesTimeLayout(rule.time_format);
    card.querySelectorAll('.cooldown-detection-time-format-datetime').forEach((field) => {
      field.hidden = !usesLayout;
    });
    const dynamic = card.querySelector('.cooldown-detection-dynamic');
    if (dynamic) dynamic.classList.toggle('cooldown-detection-dynamic--compact', !usesLayout);
  }

  function usesTimeLayout(timeFormat) {
    return timeFormat === DEFAULT_TIME_FORMAT || timeFormat === TIME_FORMAT_TIME_OF_DAY;
  }

  function showError(message) {
    if (!hasDocument) return;
    const element = document.getElementById('cooldownDetectionError');
    if (!element) return;
    element.textContent = message;
    element.hidden = false;
  }

  function hideError() {
    if (!hasDocument) return;
    const element = document.getElementById('cooldownDetectionError');
    if (!element) return;
    element.textContent = '';
    element.hidden = true;
  }

  function clearTestResult() {
    if (!hasDocument) return;
    const element = document.getElementById('cooldownDetectionTestResult');
    if (!element) return;
    element.textContent = '';
    element.hidden = true;
  }

  function showTestResult(data) {
    if (!hasDocument) return;
    const element = document.getElementById('cooldownDetectionTestResult');
    if (!element) return;
    const fields = [];
    const effectiveStatus = Number(data.status_code);
    if (Number.isInteger(effectiveStatus) && effectiveStatus >= 100 && effectiveStatus <= 599) {
      fields.push(`${t('channels.cooldownDetection.testEffectiveStatus', 'Effective status code')}: ${effectiveStatus}`);
    }
    if (data.parsed_log) {
      fields.push(t('channels.cooldownDetection.testParsedLog', 'Parsed standard upstream log'));
    }
    if (data.actionable) {
      fields.push(t('channels.cooldownDetection.testMatched', 'Matched configured rule'));
      fields.push(`${t('channels.cooldownDetection.priority', 'Priority')} ${Number(data.priority) + 1}`);
      fields.push(`${t('channels.cooldownDetection.scope', 'Cooldown scope')}: ${scopeLabel(data.scope)}`);
      if (data.cooldown_until) fields.push(`${t('channels.cooldownDetection.until', 'Until')}: ${formatTime(data.cooldown_until)}`);
    } else if (data.matched) {
      fields.push(t('channels.cooldownDetection.testFallbackMatched', 'Rule matched, but its dynamic reset time is unusable; built-in classification will handle this response.'));
    } else {
      fields.push(t('channels.cooldownDetection.testFallbackNoMatch', 'No configured rule matched; built-in classification will handle this response.'));
    }
    if (data.code) fields.push(`code: ${data.code}`);
    if (data.message) fields.push(`message: ${data.message}`);
    if (data.builtin_fallback_reason) fields.push(`${t('channels.cooldownDetection.reason', 'Reason')}: ${data.builtin_fallback_reason}`);
    if (data.captures && Object.keys(data.captures).length > 0) fields.push(`captures: ${JSON.stringify(data.captures)}`);
    element.textContent = fields.join('\n');
    element.hidden = false;
    element.classList.toggle('cooldown-detection-test-result--fallback', !data.actionable);
  }

  function scopeLabel(scope) {
    const labels = {
      key: t('channels.cooldownDetection.scopeKey', 'Current key'),
      model: t('channels.cooldownDetection.scopeModel', 'Actual model'),
      channel: t('channels.cooldownDetection.scopeChannel', 'Entire channel')
    };
    return labels[scope] || text(scope);
  }

  function formatTime(value) {
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? text(value) : parsed.toLocaleString();
  }

  function validateCooldownDetectionDraft() {
    if (!_draft) return true;
    const errors = validateCooldownDetectionRulesLocally(_draft);
    if (errors.length > 0) {
      showError(errors.join(' · '));
      return false;
    }
    return true;
  }

  function commitCooldownDetectionRules() {
    if (!validateCooldownDetectionDraft()) return false;
    if (!_draft) return true;
    _state = cloneRules(_draft);
    if (hasWindow) {
      window.channelCooldownDetectionState = _state;
      if (typeof window.markChannelFormDirty === 'function') window.markChannelFormDirty();
    }
    updateRuleCount(_state);
    return true;
  }

  async function testCooldownDetectionRules() {
    if (!_draft) return;
    const errors = validateCooldownDetectionRulesLocally(_draft);
    if (errors.length > 0) {
      showError(errors.join(' · '));
      return;
    }
    const statusInput = document.getElementById('cooldownDetectionTestStatus');
    const bodyInput = document.getElementById('cooldownDetectionTestBody');
    const testButton = document.getElementById('cooldownDetectionTestButton');
    const statusCode = Number(statusInput && statusInput.value);
    if (!Number.isInteger(statusCode) || statusCode < 100 || statusCode > 599) {
      showError(t('channels.cooldownDetection.errTestStatus', 'Test status code must be between 100 and 599'));
      return;
    }
    if (!hasWindow || typeof window.fetchDataWithAuth !== 'function') {
      showError(t('channels.cooldownDetection.testUnavailable', 'Rule test is unavailable'));
      return;
    }
    hideError();
    clearTestResult();
    if (testButton) testButton.disabled = true;
    try {
      const data = await window.fetchDataWithAuth('/admin/channels/cooldown-detection/test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          cooldown_detection_rules: { rules: serializeRules(_draft) },
          status_code: statusCode,
          error_body: bodyInput ? bodyInput.value : ''
        })
      });
      showTestResult(data || {});
    } catch (error) {
      showError(error && error.message ? error.message : t('channels.cooldownDetection.testFailed', 'Rule test failed'));
    } finally {
      if (testButton) testButton.disabled = false;
    }
  }

  function init() {
    resetCooldownDetectionState(null);
  }

  if (hasDocument) {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', init);
    } else {
      init();
    }
  }

  if (hasWindow) {
    window.beginCooldownDetectionDraft = beginCooldownDetectionDraft;
    window.discardCooldownDetectionDraft = discardCooldownDetectionDraft;
    window.addCooldownDetectionRule = addCooldownDetectionRule;
    window.removeCooldownDetectionRule = removeCooldownDetectionRule;
    window.moveCooldownDetectionRule = moveCooldownDetectionRule;
    window.setCooldownDetectionRulePriority = setCooldownDetectionRulePriority;
    window.validateCooldownDetectionDraft = validateCooldownDetectionDraft;
    window.commitCooldownDetectionRules = commitCooldownDetectionRules;
    window.testCooldownDetectionRules = testCooldownDetectionRules;
    window.resetCooldownDetectionState = resetCooldownDetectionState;
    window.collectCooldownDetectionRulesForSubmit = collectCooldownDetectionRulesForSubmit;
    window.validateCooldownDetectionRulesForSubmit = validateCooldownDetectionRulesForSubmit;
    window.validateCooldownDetectionRulesLocally = validateCooldownDetectionRulesLocally;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
      MAX_RULES,
      cloneRules,
      parseStatusCodes,
      validateCooldownDetectionRulesLocally,
      resetCooldownDetectionState,
      collectCooldownDetectionRulesForSubmit,
      validateCooldownDetectionRulesForSubmit,
      getState
    };
  }
})();
