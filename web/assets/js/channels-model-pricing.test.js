const test = require('node:test');
const assert = require('node:assert/strict');

const {
  channelModelPricingLookupCandidates,
  normalizeChannelModelPricing,
  parseChannelModelPricingForm
} = require('./channels-model-pricing.js');

test('normalize keeps only valid contract prices, including explicit zeros', () => {
  assert.deepEqual(normalizeChannelModelPricing({
    input_price: 1.5, output_price: 0, cache_read_price: -1, input_price_high: '3', token_pricing_tiers: []
  }), { input_price: 1.5, output_price: 0 });
  assert.equal(normalizeChannelModelPricing({}), null);
});

test('form parsing omits blank fields and requires input/output pairs', () => {
  assert.deepEqual(parseChannelModelPricingForm({ input_price: ' 2 ', output_price: '0', cache_read_price: '' }),
    { pricing: { input_price: 2, output_price: 0 }, error: '', field: '' });
  assert.deepEqual(parseChannelModelPricingForm({}), { pricing: null, error: '', field: '' });
  assert.equal(parseChannelModelPricingForm({ input_price: '-1', output_price: '1' }).error,
    'channels.modelPricing.invalidNumber');
  assert.equal(parseChannelModelPricingForm({ input_price: '1' }).field, 'output_price');
  assert.equal(parseChannelModelPricingForm({ input_price: '1', output_price: '2', cache_read_price_high: '3' }).field,
    'input_price_high');
});

test('prefill looks up the redirect target before the channel model name', () => {
  assert.deepEqual(channelModelPricingLookupCandidates({ model: 'alias', redirect_model: 'upstream' }), ['upstream', 'alias']);
  assert.deepEqual(channelModelPricingLookupCandidates({ model: '*' }), []);
});
