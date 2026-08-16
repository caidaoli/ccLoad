const test = require('node:test');
const assert = require('node:assert/strict');

const {
  buildRequestPayload,
  normalizeImages,
  dataURLFromImage,
  imageSizeOptions
} = require('./model-test-image.js');

test('image generation payload preserves the wire contract and omits automatic options', () => {
  assert.deepEqual(buildRequestPayload({
    generationAPI: 'chat_completions',
    model: ' gpt-image-2 ',
    prompt: ' a white cat ',
    keyIndex: '3',
    size: '3:2@2k',
    quality: 'high',
    background: 'auto',
    outputFormat: 'webp'
  }), {
    generation_api: 'chat_completions',
    model: 'gpt-image-2',
    prompt: 'a white cat',
    key_index: 3,
    size: '3:2@2k'
  });

  assert.deepEqual(buildRequestPayload({
    model: 'gpt-image-2',
    prompt: 'a white cat',
    keyIndex: ''
  }), {
    generation_api: 'images',
    model: 'gpt-image-2',
    prompt: 'a white cat'
  });
});

test('image generation size options expose native contracts for each API', () => {
  assert.ok(imageSizeOptions('images').some(([value]) => value === '512x512'));
  assert.ok(!imageSizeOptions('chat_completions').some(([value]) => value === '512x512'));
  assert.ok(imageSizeOptions('chat_completions').some(([value]) => value === '3:2@2k'));
  assert.ok(imageSizeOptions('images', true).some(([value]) => value === '3:2@2k'));
});

test('image generation response accepts URL and base64 images but rejects empty entries', () => {
  const images = normalizeImages({
    images: [
      { url: 'https://example.com/image.png' },
      { b64_json: 'aW1hZ2U=' },
      { url: 'javascript:alert(1)' },
      { revised_prompt: 'missing image data' }
    ]
  });

  assert.equal(images.length, 2);
  assert.equal(dataURLFromImage(images[0], 'png'), 'https://example.com/image.png');
  assert.equal(dataURLFromImage(images[1], 'jpeg'), 'data:image/jpeg;base64,aW1hZ2U=');
  assert.equal(dataURLFromImage({ b64_json: 'aW1hZ2U=', mime_type: 'image/webp' }, 'png'), 'data:image/webp;base64,aW1hZ2U=');
});

test('image option controls reuse searchable comboboxes and keep API capability linkage', () => {
  const elements = new Map();
  const makeElement = () => {
    const listeners = new Map();
    return {
      value: '',
      disabled: false,
      placeholder: '',
      classList: { toggle() {} },
      setAttribute() {},
      addEventListener(type, listener) { listeners.set(type, listener); },
      dispatch(type, event = {}) { listeners.get(type)?.(event); },
      focus() {},
      querySelector() { return null; }
    };
  };
  const document = {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, makeElement());
      return elements.get(id);
    }
  };
  const comboboxes = new Map();
  const storage = new Map([
    ['ccload_model_test_image_generation_api', 'images'],
    ['ccload_model_test_image_size_images', '1536x1024'],
    ['ccload_model_test_image_quality', 'hd'],
    ['ccload_model_test_image_background', 'transparent'],
    ['ccload_model_test_image_output_format', 'webp'],
    ['ccload_model_test_image_prompt', 'persisted prompt']
  ]);
  const fakeWindow = {
    document,
    localStorage: {
      getItem: key => storage.has(key) ? storage.get(key) : null,
      setItem: (key, value) => storage.set(key, String(value))
    },
    t: () => '',
    createSearchableCombobox(config) {
      let value = config.initialValue || '';
      const input = document.getElementById(config.inputId);
      const instance = {
        config,
        getValue: () => value,
        setValue(nextValue, label) {
          value = nextValue;
          input.value = label;
        },
        refresh() {},
        getInput: () => input,
        select(nextValue) {
          value = nextValue;
          config.onSelect?.(nextValue, nextValue);
        }
      };
      comboboxes.set(config.inputId, instance);
      return instance;
    }
  };

  const modulePath = require.resolve('./model-test-image.js');
  const previousWindow = global.window;
  delete require.cache[modulePath];
  global.window = fakeWindow;
  try {
    const freshModule = require(modulePath);
    freshModule.init({
      getModelOptions: () => ['gpt-image-2'],
      getChannelsForModel: () => [{ id: 7, name: 'Images', auth_type: 'api_key' }]
    });

    for (const id of [
      'imageGenerationAPISelect',
      'imageSizeSelect',
      'imageQualitySelect',
      'imageBackgroundSelect',
      'imageOutputFormatSelect'
    ]) {
      assert.ok(comboboxes.has(id), `${id} must use the shared searchable combobox`);
    }
    assert.equal(comboboxes.get('imageSizeSelect').getValue(), '1536x1024');
    assert.equal(comboboxes.get('imageQualitySelect').getValue(), 'hd');
    assert.equal(comboboxes.get('imageBackgroundSelect').getValue(), 'transparent');
    assert.equal(comboboxes.get('imageOutputFormatSelect').getValue(), 'webp');
    assert.equal(elements.get('imagePrompt').value, 'persisted prompt');

    elements.get('imagePrompt').value = 'updated prompt';
    elements.get('imagePrompt').dispatch('input');
    assert.equal(storage.get('ccload_model_test_image_prompt'), 'updated prompt');

    comboboxes.get('imageGenerationAPISelect').select('chat_completions');
    const chatSizes = comboboxes.get('imageSizeSelect').config.getOptions().map(option => option.value);
    assert.ok(chatSizes.includes('3:2@2k'));
    assert.ok(!chatSizes.includes('512x512'));
    assert.equal(elements.get('imageQualitySelect').disabled, true);

    comboboxes.get('imageGenerationAPISelect').select('images');
    const imageSizes = comboboxes.get('imageSizeSelect').config.getOptions().map(option => option.value);
    assert.ok(imageSizes.includes('512x512'));
    assert.equal(elements.get('imageQualitySelect').disabled, false);
  } finally {
    delete require.cache[modulePath];
    if (previousWindow === undefined) delete global.window;
    else global.window = previousWindow;
  }
});
