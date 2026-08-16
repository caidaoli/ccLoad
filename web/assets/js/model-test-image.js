(function (root, factory) {
  const api = factory(root);
  if (typeof module === 'object' && module.exports) module.exports = api;
  if (root) root.ModelTestImage = api;
})(typeof window !== 'undefined' ? window : globalThis, function (root) {
  'use strict';

  const STORAGE_PREFIX = 'ccload_model_test_image_';
  const IMAGES_SIZE_OPTIONS = [
    ['auto', '自动'],
    ['1024x1024', '1024 × 1024'],
    ['1536x1024', '1536 × 1024'],
    ['1024x1536', '1024 × 1536'],
    ['1792x1024', '1792 × 1024'],
    ['1024x1792', '1024 × 1792'],
    ['512x512', '512 × 512'],
    ['256x256', '256 × 256']
  ];
  const CHAT_SIZE_OPTIONS = [
    ['auto', '自动'],
    ['1:1@1k', '1:1 · 1K'],
    ['1:1@2k', '1:1 · 2K'],
    ['16:9@1k', '16:9 · 1K'],
    ['16:9@2k', '16:9 · 2K'],
    ['9:16@1k', '9:16 · 1K'],
    ['9:16@2k', '9:16 · 2K'],
    ['3:2@1k', '3:2 · 1K'],
    ['3:2@2k', '3:2 · 2K'],
    ['2:3@1k', '2:3 · 1K'],
    ['2:3@2k', '2:3 · 2K']
  ];
  const GENERATION_API_OPTIONS = [
    ['images', 'Images API', 'modelTest.image.generationApiImages'],
    ['chat_completions', 'Chat Completions', 'modelTest.image.generationApiChat']
  ];
  const QUALITY_OPTIONS = [
    ['auto', '自动', 'modelTest.image.auto'],
    ['low', '低', 'modelTest.image.qualityLow'],
    ['medium', '中', 'modelTest.image.qualityMedium'],
    ['high', '高', 'modelTest.image.qualityHigh'],
    ['standard', '标准', 'modelTest.image.qualityStandard'],
    ['hd', '高清', 'modelTest.image.qualityHD']
  ];
  const BACKGROUND_OPTIONS = [
    ['auto', '自动', 'modelTest.image.auto'],
    ['opaque', '不透明', 'modelTest.image.backgroundOpaque'],
    ['transparent', '透明', 'modelTest.image.backgroundTransparent']
  ];
  const OUTPUT_FORMAT_OPTIONS = [
    ['auto', '自动', 'modelTest.image.auto'],
    ['png', 'PNG'],
    ['jpeg', 'JPEG'],
    ['webp', 'WebP']
  ];
  let dependencies = {};
  let channels = [];
  let initialized = false;
  let requestID = 0;
  let submitting = false;
  let modelCombobox = null;
  let channelCombobox = null;
  let keyCombobox = null;
  let generationAPICombobox = null;
  let sizeCombobox = null;
  let qualityCombobox = null;
  let backgroundCombobox = null;
  let outputFormatCombobox = null;
  let currentSizeOptions = IMAGES_SIZE_OPTIONS;
  let channelKeys = [];
  let preferredChannelID = '';

  function text(key, fallback, params) {
    if (typeof dependencies.t === 'function') return dependencies.t(key, fallback, params);
    if (typeof root?.t === 'function') return root.t(key, params) || fallback;
    return fallback;
  }

  function comboboxOptions(definitions) {
    return definitions.map(([value, fallback, key]) => ({
      value,
      label: key ? text(key, fallback) : fallback
    }));
  }

  function setComboboxSelection(combobox, inputID, definitions, value, fallback = '') {
    const options = comboboxOptions(definitions);
    const selected = options.find(option => option.value === value)
      || options.find(option => option.value === fallback)
      || options[0]
      || { value: '', label: '' };
    combobox?.setValue(selected.value, selected.label);
    combobox?.refresh();
    const input = combobox?.getInput?.() || element(inputID);
    if (input && !combobox) input.value = selected.label;
    return selected.value;
  }

  function storageGet(key, fallback = '') {
    try {
      const value = root?.localStorage?.getItem(STORAGE_PREFIX + key);
      return value === null ? fallback : value;
    } catch (_) {
      return fallback;
    }
  }

  function storageSet(key, value) {
    try {
      root?.localStorage?.setItem(STORAGE_PREFIX + key, String(value ?? ''));
    } catch (_) { /* ignore */ }
  }

  function buildRequestPayload(values) {
    const generationAPI = String(values?.generationAPI || 'images').trim().toLowerCase();
    const payload = {
      generation_api: generationAPI,
      model: String(values?.model || '').trim(),
      prompt: String(values?.prompt || '').trim()
    };
    const rawKeyIndex = String(values?.keyIndex ?? '').trim();
    const keyIndex = Number(rawKeyIndex);
    if (rawKeyIndex && Number.isInteger(keyIndex) && keyIndex >= 0) payload.key_index = keyIndex;

    const size = String(values?.size || '').trim().toLowerCase();
    if (size && size !== 'auto') payload.size = size;
    if (generationAPI === 'images' && values?.supportsExtendedOptions !== false) {
      for (const [inputKey, outputKey] of [
        ['quality', 'quality'],
        ['background', 'background'],
        ['outputFormat', 'output_format']
      ]) {
        const value = String(values?.[inputKey] || '').trim().toLowerCase();
        if (value && value !== 'auto') payload[outputKey] = value;
      }
    }
    return payload;
  }

  function safeRemoteImageURL(value) {
    if (typeof value !== 'string' || !value.trim()) return '';
    try {
      const parsed = new URL(value.trim());
      return parsed.protocol === 'https:' || parsed.protocol === 'http:' ? parsed.href : '';
    } catch (_) {
      return '';
    }
  }

  function normalizeImages(data) {
    if (!Array.isArray(data?.images)) return [];
    return data.images.filter((image) => image && (
      safeRemoteImageURL(image.url) ||
      typeof image.b64_json === 'string' && image.b64_json.trim()
    ));
  }

  function imageMIMEType(outputFormat, explicitMIMEType) {
    const mimeType = String(explicitMIMEType || '').trim().toLowerCase();
    if (['image/png', 'image/jpeg', 'image/webp', 'image/gif'].includes(mimeType)) return mimeType;
    const format = String(outputFormat || '').trim().toLowerCase();
    if (format === 'jpeg' || format === 'jpg') return 'image/jpeg';
    if (format === 'webp') return 'image/webp';
    return 'image/png';
  }

  function dataURLFromImage(image, outputFormat) {
    const remoteURL = safeRemoteImageURL(image?.url);
    if (remoteURL) return remoteURL;
    if (typeof image?.b64_json !== 'string' || !image.b64_json.trim()) return '';
    return `data:${imageMIMEType(outputFormat, image?.mime_type)};base64,${image.b64_json.trim()}`;
  }

  function outputFormatFromMIMEType(value) {
    switch (String(value || '').trim().toLowerCase()) {
      case 'image/jpeg': return 'jpeg';
      case 'image/webp': return 'webp';
      case 'image/gif': return 'gif';
      case 'image/png': return 'png';
      default: return '';
    }
  }

  function element(id) {
    return root?.document?.getElementById(id) || null;
  }

  function getModelName(entry) {
    if (typeof dependencies.getModelName === 'function') return dependencies.getModelName(entry);
    return typeof entry === 'string' ? entry : String(entry?.model || '');
  }

  function modelOptions() {
    if (typeof dependencies.getModelOptions === 'function') {
      return dependencies.getModelOptions();
    }
    const models = new Set();
    channels.forEach((channel) => {
      (channel?.models || []).forEach((entry) => {
        const name = String(getModelName(entry) || '').trim();
        if (name) models.add(name);
      });
    });
    return Array.from(models).sort((a, b) => a.localeCompare(b));
  }

  function channelSupportsModel(channel, model) {
    if (!model) return true;
    if (typeof dependencies.isModelSupported === 'function') {
      return dependencies.isModelSupported(channel, model);
    }
    return (channel?.models || []).some(entry => getModelName(entry) === model);
  }

  function currentModel() {
    return String(modelCombobox?.getValue?.() || element('imageModelInput')?.value || '').trim();
  }

  function currentGenerationAPI() {
    return String(generationAPICombobox?.getValue?.() || storageGet('generation_api', 'images')).trim().toLowerCase();
  }

  function selectedChannel() {
    const selectedID = String(channelCombobox?.getValue?.() || preferredChannelID || '');
    return channels.find(channel => String(channel?.id) === selectedID) || null;
  }

  function supportsExtendedImageOptions() {
    return currentGenerationAPI() === 'images' && selectedChannel()?.auth_type !== 'xai_oauth';
  }

  function usesNativeImageSize() {
    return currentGenerationAPI() === 'chat_completions' || selectedChannel()?.auth_type === 'xai_oauth';
  }

  function imageSizeOptions(generationAPI, nativeImages = false) {
    return generationAPI === 'chat_completions' || nativeImages ? CHAT_SIZE_OPTIONS : IMAGES_SIZE_OPTIONS;
  }

  function imageSizeStorageKey() {
    return usesNativeImageSize() ? 'size_native' : 'size_images';
  }

  function currentSizeOptionDefinitions() {
    return currentSizeOptions.map(([value, label]) => [
      value,
      label,
      value === 'auto' ? 'modelTest.image.auto' : ''
    ]);
  }

  function syncSizeOptions() {
    const generationAPI = currentGenerationAPI();
    currentSizeOptions = imageSizeOptions(generationAPI, usesNativeImageSize());
    const stored = storageGet(imageSizeStorageKey(), 'auto');
    const selected = currentSizeOptions.some(([value]) => value === stored) ? stored : 'auto';
    setComboboxSelection(sizeCombobox, 'imageSizeSelect', currentSizeOptionDefinitions(), selected, 'auto');
  }

  function syncCapabilityControls() {
    const supported = supportsExtendedImageOptions();
    for (const { id, storageKey, combobox, options } of [
      { id: 'imageQualitySelect', storageKey: 'quality', combobox: qualityCombobox, options: QUALITY_OPTIONS },
      { id: 'imageBackgroundSelect', storageKey: 'background', combobox: backgroundCombobox, options: BACKGROUND_OPTIONS },
      { id: 'imageOutputFormatSelect', storageKey: 'output_format', combobox: outputFormatCombobox, options: OUTPUT_FORMAT_OPTIONS }
    ]) {
      const field = combobox?.getInput?.() || element(id);
      if (!field) continue;
      field.disabled = !supported;
      field.setAttribute('aria-disabled', supported ? 'false' : 'true');
      const stored = supported ? storageGet(storageKey, 'auto') : 'auto';
      setComboboxSelection(combobox, id, options, stored, 'auto');
    }
  }

  function syncGenerationAPIControls() {
    syncSizeOptions();
    syncCapabilityControls();
  }

  function availableChannels() {
    const model = currentModel();
    if (typeof dependencies.getChannelsForModel === 'function') {
      return dependencies.getChannelsForModel(model);
    }
    return channels.filter(channel => channelSupportsModel(channel, model));
  }

  function formatChannelLabel(channel) {
    if (typeof dependencies.formatChannelLabel === 'function') return dependencies.formatChannelLabel(channel);
    return String(channel?.name || `#${channel?.id ?? '?'}`);
  }

  function channelOptionClass(channel) {
    if (typeof dependencies.getChannelOptionClass === 'function') return dependencies.getChannelOptionClass(channel);
    return channel?.enabled === false ? 'filter-dropdown-item--disabled' : '';
  }

  function keyOptionClass(key) {
    if (typeof dependencies.getKeyOptionClass === 'function') return dependencies.getKeyOptionClass(key);
    return key?.disabled === true ? 'filter-dropdown-item--disabled' : '';
  }

  function syncModelOptions() {
    const models = modelOptions();
    const selected = currentModel() || storageGet('model') || models[0] || '';
    modelCombobox?.setValue(selected, selected);
    modelCombobox?.refresh();
  }

  function syncChannelOptions() {
    const available = availableChannels();
    const selectedChannel = available.find(channel => String(channel.id) === preferredChannelID) || available[0] || null;
    const selectedID = selectedChannel ? String(selectedChannel.id) : '';
    channelCombobox?.setValue(selectedID, selectedChannel ? formatChannelLabel(selectedChannel) : '');
    channelCombobox?.refresh();
    syncGenerationAPIControls();
    void syncKeyOptions(selectedID);
  }

  function formatKeyLabel(key) {
    if (typeof dependencies.formatKeyLabel === 'function') return dependencies.formatKeyLabel(key);
    const raw = String(key?.api_key || '').trim();
    if (raw.length <= 6) return raw || `#${key?.key_index ?? '?'}`;
    return `${raw.slice(0, 3)}.${raw.slice(-3)}`;
  }

  async function syncKeyOptions(channelID) {
    const currentRequestID = ++requestID;
    channelKeys = [];
    keyCombobox?.setValue('', '');
    keyCombobox?.refresh();
    const input = keyCombobox?.getInput?.() || element('imageKeySelect');
    if (!channelID) {
      if (input) input.placeholder = text('modelTest.image.selectChannel', '请选择渠道');
      return;
    }
    if (input) input.placeholder = text('modelTest.image.loadingKeys', '加载 API Key...');
    if (typeof dependencies.getChannelKeys !== 'function') return;
    try {
      const keys = await dependencies.getChannelKeys(Number(channelID));
      if (currentRequestID !== requestID) return;
      channelKeys = Array.isArray(keys) ? keys : [];
      const preferredIndex = storageGet(`key_index_${channelID}`);
      const selectedKey = channelKeys.find(key => key?.disabled !== true && String(key.key_index) === preferredIndex)
        || channelKeys.find(key => key?.disabled !== true)
        || null;
      const selectedIndex = selectedKey ? String(selectedKey.key_index) : '';
      keyCombobox?.setValue(selectedIndex, selectedKey ? formatKeyLabel(selectedKey) : '');
      keyCombobox?.refresh();
      if (input) {
        input.placeholder = selectedKey
          ? text('channels.selectApiKey', '选择 API Key')
          : text('modelTest.image.noKeys', '没有可用 API Key');
      }
      if (selectedIndex) storageSet(`key_index_${channelID}`, selectedIndex);
    } catch (error) {
      if (currentRequestID !== requestID) return;
      if (input) input.placeholder = text('modelTest.image.loadKeysFailed', 'API Key 加载失败');
    }
  }

  function initTargetComboboxes() {
    if (typeof root?.createSearchableCombobox !== 'function') return;
    const storedModel = storageGet('model');
    preferredChannelID = storageGet('channel_id');
    modelCombobox = root.createSearchableCombobox({
      attachMode: true,
      inputId: 'imageModelInput',
      dropdownId: 'imageModelDropdown',
      allowCustomInput: true,
      initialValue: storedModel,
      initialLabel: storedModel,
      getOptions: () => modelOptions().map(model => ({ value: model, label: model })),
      onSelect: (value) => {
        storageSet('model', String(value || '').trim());
        syncChannelOptions();
      }
    });

    channelCombobox = root.createSearchableCombobox({
      attachMode: true,
      inputId: 'imageChannelSelect',
      dropdownId: 'imageChannelDropdown',
      initialValue: preferredChannelID,
      initialLabel: '',
      getOptions: () => availableChannels().map(channel => ({
        value: String(channel.id),
        label: formatChannelLabel(channel),
        className: channelOptionClass(channel)
      })),
      onSelect: (value) => {
        const channelID = String(value || '');
        preferredChannelID = channelID;
        storageSet('channel_id', channelID);
        syncGenerationAPIControls();
        void syncKeyOptions(channelID);
      }
    });

    keyCombobox = root.createSearchableCombobox({
      attachMode: true,
      inputId: 'imageKeySelect',
      dropdownId: 'imageKeyDropdown',
      initialValue: '',
      initialLabel: '',
      getOptions: () => channelKeys.map(key => ({
        value: String(key.key_index),
        label: formatKeyLabel(key),
        className: keyOptionClass(key)
      })),
      onSelect: (value) => {
        const channelID = String(channelCombobox?.getValue?.() || '');
        if (channelID) storageSet(`key_index_${channelID}`, value);
      }
    });
  }

  function createOptionCombobox(inputId, dropdownId, getDefinitions, onSelect) {
    return root.createSearchableCombobox({
      attachMode: true,
      inputId,
      dropdownId,
      initialValue: '',
      initialLabel: '',
      getOptions: () => comboboxOptions(getDefinitions()),
      onSelect
    });
  }

  function initOptionComboboxes() {
    if (typeof root?.createSearchableCombobox !== 'function') return;
    generationAPICombobox = createOptionCombobox(
      'imageGenerationAPISelect',
      'imageGenerationAPISelectDropdown',
      () => GENERATION_API_OPTIONS,
      (value) => {
        storageSet('generation_api', value);
        syncGenerationAPIControls();
      }
    );
    sizeCombobox = createOptionCombobox(
      'imageSizeSelect',
      'imageSizeSelectDropdown',
      currentSizeOptionDefinitions,
      (value) => storageSet(imageSizeStorageKey(), value)
    );
    qualityCombobox = createOptionCombobox(
      'imageQualitySelect',
      'imageQualitySelectDropdown',
      () => QUALITY_OPTIONS,
      (value) => storageSet('quality', value)
    );
    backgroundCombobox = createOptionCombobox(
      'imageBackgroundSelect',
      'imageBackgroundSelectDropdown',
      () => BACKGROUND_OPTIONS,
      (value) => storageSet('background', value)
    );
    outputFormatCombobox = createOptionCombobox(
      'imageOutputFormatSelect',
      'imageOutputFormatSelectDropdown',
      () => OUTPUT_FORMAT_OPTIONS,
      (value) => storageSet('output_format', value)
    );
  }

  function setStatus(message, error = false) {
    const status = element('imageGenerationStatus');
    const errorRegion = element('imageGenerationError');
    const feedback = element('imageGenerationFeedback');
    if (status) status.textContent = error ? '' : message;
    if (errorRegion) errorRegion.textContent = error ? message : '';
    if (feedback) feedback.hidden = !message;
  }

  function setBusy(busy) {
    const form = element('imageGenerationForm');
    const results = element('imageGenerationResults');
    const button = element('imageGenerateBtn');
    form?.setAttribute('aria-busy', busy ? 'true' : 'false');
    results?.setAttribute('aria-busy', busy ? 'true' : 'false');
    if (button) {
      button.disabled = busy;
      button.classList.toggle('is-loading', busy);
      const label = button.querySelector('span');
      if (label) label.textContent = busy
        ? text('modelTest.image.generating', '生成中...')
        : text('modelTest.image.generate', '生成图片');
    }
  }

  function renderResults(data) {
    const container = element('imageGenerationResults');
    const summary = element('imageGenerationSummary');
    if (!container) return;
    const images = normalizeImages(data);
    container.replaceChildren();
    const outputFormat = data?.output_format || 'png';

    images.forEach((image, index) => {
      const imageOutputFormat = outputFormatFromMIMEType(image?.mime_type) || outputFormat;
      const source = dataURLFromImage(image, imageOutputFormat);
      const figure = root.document.createElement('figure');
      figure.className = 'image-test-result-card';

      const preview = root.document.createElement('img');
      preview.className = 'image-test-result-image';
      preview.src = source;
      preview.alt = String(image.revised_prompt || element('imagePrompt')?.value || '').trim()
        || text('modelTest.image.resultAlt', `生成图片 ${index + 1}`, { index: index + 1 });
      preview.loading = 'lazy';
      preview.decoding = 'async';
      preview.referrerPolicy = 'no-referrer';
      figure.appendChild(preview);

      const caption = root.document.createElement('figcaption');
      caption.className = 'image-test-result-caption';
      if (typeof image.revised_prompt === 'string' && image.revised_prompt.trim()) {
        const revisedPrompt = root.document.createElement('p');
        revisedPrompt.className = 'image-test-revised-prompt';
        revisedPrompt.textContent = image.revised_prompt.trim();
        caption.appendChild(revisedPrompt);
      }

      const link = root.document.createElement('a');
      link.className = 'btn btn-secondary image-test-download';
      link.href = source;
      link.textContent = image.b64_json
        ? text('modelTest.image.download', '下载')
        : text('modelTest.image.openOriginal', '打开原图');
      if (image.b64_json) {
        const extension = imageOutputFormat === 'jpeg' ? 'jpg' : imageOutputFormat;
        link.download = `generated-image-${index + 1}.${extension}`;
      } else {
        link.target = '_blank';
        link.rel = 'noopener noreferrer';
      }
      caption.appendChild(link);
      figure.appendChild(caption);
      container.appendChild(figure);
    });

    const summaryParts = [
      text('modelTest.image.resultCount', `${images.length} 张图片`, { count: images.length })
    ];
    if (Number.isFinite(Number(data?.duration_ms))) {
      summaryParts.push(text('modelTest.image.duration', `${Number(data.duration_ms)} ms`, { duration: Number(data.duration_ms) }));
    }
    if (Number(data?.cost_usd) > 0) summaryParts.push(`$${Number(data.cost_usd).toFixed(6)}`);
    if (summary) summary.textContent = summaryParts.join(' · ');
  }

  function currentValues() {
    return {
      model: currentModel(),
      prompt: element('imagePrompt')?.value,
      channelID: String(channelCombobox?.getValue?.() || ''),
      keyIndex: String(keyCombobox?.getValue?.() || ''),
      generationAPI: currentGenerationAPI(),
      size: String(sizeCombobox?.getValue?.() || 'auto'),
      quality: String(qualityCombobox?.getValue?.() || 'auto'),
      background: String(backgroundCombobox?.getValue?.() || 'auto'),
      outputFormat: String(outputFormatCombobox?.getValue?.() || 'auto'),
      supportsExtendedOptions: supportsExtendedImageOptions()
    };
  }

  async function submit(event) {
    event?.preventDefault();
    if (submitting) return;
    const form = element('imageGenerationForm');
    const values = currentValues();
    if (!values.channelID) {
      setStatus(text('modelTest.image.selectChannelError', '请先选择渠道'), true);
      element('imageChannelSelect')?.focus();
      return;
    }
    if (!form?.reportValidity()) return;

    const payload = buildRequestPayload(values);
    storageSet('model', payload.model);
    storageSet(`key_index_${values.channelID}`, values.keyIndex);
    storageSet('generation_api', values.generationAPI);
    storageSet(imageSizeStorageKey(), values.size);
    if (values.supportsExtendedOptions) {
      storageSet('quality', values.quality);
      storageSet('background', values.background);
      storageSet('output_format', values.outputFormat);
    }

    submitting = true;
    setStatus(text('modelTest.image.generatingStatus', '正在生成图片...'));
    setBusy(true);
    try {
      const data = await root.fetchDataWithAuth(`/admin/channels/${values.channelID}/images/generations`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      if (!data?.success) throw new Error(data?.error || text('modelTest.image.failed', '图片生成失败'));
      renderResults(data);
      setStatus(text('modelTest.image.success', '图片生成完成'));
    } catch (error) {
      setStatus(error?.message || text('modelTest.image.failed', '图片生成失败'), true);
    } finally {
      submitting = false;
      setBusy(false);
    }
  }

  function restoreOptions() {
    const storedAPI = storageGet('generation_api', 'images');
    setComboboxSelection(
      generationAPICombobox,
      'imageGenerationAPISelect',
      GENERATION_API_OPTIONS,
      storedAPI,
      'images'
    );
    syncGenerationAPIControls();
  }

  function init(nextDependencies = {}) {
    dependencies = { ...dependencies, ...nextDependencies };
    if (initialized || !root?.document) return;
    initialized = true;
    initTargetComboboxes();
    initOptionComboboxes();
    restoreOptions();
    element('imageGenerationForm')?.addEventListener('submit', submit);
    element('imagePrompt')?.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
        event.preventDefault();
        element('imageGenerationForm')?.requestSubmit();
      }
    });
    syncModelOptions();
    syncChannelOptions();
  }

  function setChannels(nextChannels) {
    channels = Array.isArray(nextChannels) ? nextChannels.slice() : [];
    if (!initialized) return;
    syncModelOptions();
    syncChannelOptions();
  }

  function open() {
    syncModelOptions();
    syncChannelOptions();
  }

  return {
    buildRequestPayload,
    normalizeImages,
    dataURLFromImage,
    imageSizeOptions,
    init,
    setChannels,
    open
  };
});
