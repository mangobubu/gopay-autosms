import assert from 'node:assert/strict'
import test from 'node:test'

import {
  API_KEY_STORAGE_KEY,
  BATCH_FORM_STORAGE_KEY,
  CLIENT_PERSISTENCE_VERSION,
  DEFAULT_BATCH_FORM,
  HERO_SMS_API_KEY_STORAGE_KEY,
  createClientPersistence,
  loadApiKey,
  loadBatchForm,
  mergeClientApiKey,
  saveApiKey,
  saveBatchForm,
} from './persistence.ts'

class MemoryStorage {
  values = new Map()

  getItem(key) {
    return this.values.get(key) ?? null
  }

  setItem(key, value) {
    this.values.set(key, String(value))
  }

  removeItem(key) {
    this.values.delete(key)
  }
}

test('direct helpers persist the API key and complete batch draft in versioned envelopes', () => {
  const storage = new MemoryStorage()
  const form = {
    smsProvider: 'smsbower',
    service: 'goPay',
    country: 'ID',
    priceKey: 'offer-7',
    quantity: 12,
    pin: '123456',
    proxy: '127.0.0.1:8080',
    priceSnapshot: {
      value: 'offer-7',
      provider: '42',
      price: 1.25,
      tier: 'Gold',
      tierDerived: true,
    },
  }

  assert.equal(saveApiKey(' secret-key ', storage), true)
  assert.equal(saveBatchForm(form, storage), true)
  assert.equal(loadApiKey(storage), 'secret-key')
  assert.deepEqual(loadBatchForm(storage), form)

  assert.deepEqual(JSON.parse(storage.getItem(API_KEY_STORAGE_KEY)), {
    schema: 'gopay-autosms/api-key',
    version: CLIENT_PERSISTENCE_VERSION,
    data: 'secret-key',
  })
  assert.deepEqual(JSON.parse(storage.getItem(BATCH_FORM_STORAGE_KEY)).version, 1)
})

test('stores SMSBower and HeroSMS API keys independently while keeping the legacy key', () => {
  const storage = new MemoryStorage()

  assert.equal(saveApiKey(' smsbower-key ', storage), true)
  assert.equal(saveApiKey(' hero-key ', 'hero-sms', storage), true)
  assert.equal(loadApiKey(storage), 'smsbower-key')
  assert.equal(loadApiKey('smsbower', storage), 'smsbower-key')
  assert.equal(loadApiKey('hero-sms', storage), 'hero-key')
  assert.equal(JSON.parse(storage.getItem(API_KEY_STORAGE_KEY)).data, 'smsbower-key')
  assert.equal(JSON.parse(storage.getItem(HERO_SMS_API_KEY_STORAGE_KEY)).data, 'hero-key')
})

test('keeps client plaintext when the settings endpoint returns a masked key', () => {
  assert.equal(mergeClientApiKey(' client-secret ', 'clie*****cret'), 'client-secret')
  assert.equal(mergeClientApiKey('client-secret', ''), 'client-secret')
  assert.equal(mergeClientApiKey('old-secret', ' new-secret '), 'new-secret')
})

test('returns fresh defaults for malformed, incompatible and unavailable storage', () => {
  const storage = new MemoryStorage()
  storage.setItem(API_KEY_STORAGE_KEY, '{bad json')
  storage.setItem(BATCH_FORM_STORAGE_KEY, JSON.stringify({
    schema: 'gopay-autosms/batch-form',
    version: 99,
    data: { quantity: 50 },
  }))
  const persistence = createClientPersistence(storage)

  assert.equal(persistence.loadApiKey(), '')
  assert.deepEqual(persistence.loadBatchForm(), {
    smsProvider: 'smsbower',
    service: '',
    country: '',
    priceKey: '',
    quantity: 1,
    pin: '',
    proxy: '',
  })

  const throwingStorage = {
    getItem() { throw new Error('blocked') },
    setItem() { throw new Error('blocked') },
    removeItem() { throw new Error('blocked') },
  }
  const unavailable = createClientPersistence(throwingStorage)
  assert.equal(unavailable.loadApiKey(), '')
  assert.equal(unavailable.saveApiKey('key'), false)
  assert.equal(unavailable.clearBatchForm(), false)
})

test('sanitizes invalid individual form fields without losing valid fields', () => {
  const storage = new MemoryStorage()
  storage.setItem(BATCH_FORM_STORAGE_KEY, JSON.stringify({
    schema: 'gopay-autosms/batch-form',
    version: CLIENT_PERSISTENCE_VERSION,
    data: {
      service: 'gopay',
      country: 7,
      priceKey: 'offer',
      quantity: 101,
      pin: '12345x',
      proxy: 'proxy',
      priceSnapshot: { value: 'offer', price: Number.NaN, tier: 'Platinum' },
    },
  }))

  assert.deepEqual(createClientPersistence(storage).loadBatchForm(), {
    smsProvider: 'smsbower',
    service: 'gopay',
    country: '',
    priceKey: 'offer',
    quantity: 1,
    pin: '',
    proxy: 'proxy',
    priceSnapshot: { value: 'offer' },
  })
})

test('ignores mismatched schemas even when version and data are otherwise valid', () => {
  const storage = new MemoryStorage()
  storage.setItem(API_KEY_STORAGE_KEY, JSON.stringify({
    schema: 'gopay-autosms/batch-form',
    version: CLIENT_PERSISTENCE_VERSION,
    data: 'wrong-schema-key',
  }))
  storage.setItem(BATCH_FORM_STORAGE_KEY, JSON.stringify({
    schema: 'gopay-autosms/api-key',
    version: CLIENT_PERSISTENCE_VERSION,
    data: {
      service: 'wa', country: '62', priceKey: 'offer', quantity: 3, pin: '123456', proxy: '',
    },
  }))

  assert.equal(loadApiKey(storage), '')
  assert.deepEqual(loadBatchForm(storage), DEFAULT_BATCH_FORM)
})

test('returns fresh defaults so restored form state cannot leak between callers', () => {
  const storage = new MemoryStorage()
  const persistence = createClientPersistence(storage)
  const first = persistence.loadBatchForm()
  const second = persistence.loadBatchForm()

  assert.notEqual(first, second)
  first.service = 'changed'
  assert.deepEqual(second, DEFAULT_BATCH_FORM)
})

test('restores legacy drafts as SMSBower and keeps an explicit HeroSMS provider', () => {
  const storage = new MemoryStorage()
  storage.setItem(BATCH_FORM_STORAGE_KEY, JSON.stringify({
    schema: 'gopay-autosms/batch-form',
    version: CLIENT_PERSISTENCE_VERSION,
    data: {
      service: 'legacy-service',
      country: '6',
      priceKey: 'legacy-price',
      quantity: 2,
      pin: '123456',
      proxy: '',
    },
  }))

  assert.equal(loadBatchForm(storage).smsProvider, 'smsbower')

  saveBatchForm({
    smsProvider: 'hero-sms',
    service: 'hero-service',
    country: 'ID',
    priceKey: 'hero-price',
    quantity: 3,
    pin: '654321',
    proxy: '',
    priceSnapshot: {
      value: 'hero-price',
      price: 1,
      provider: '17',
      tier: 'Gold',
      tierDerived: false,
    },
  }, storage)

  assert.deepEqual(loadBatchForm(storage), {
    smsProvider: 'hero-sms',
    service: 'hero-service',
    country: 'ID',
    priceKey: 'hero-price',
    quantity: 3,
    pin: '654321',
    proxy: '',
    priceSnapshot: { value: 'hero-price', price: 1 },
  })
})

test('clear methods remove both client-persisted values', () => {
  const storage = new MemoryStorage()
  const persistence = createClientPersistence(storage)
  persistence.saveApiKey('secret')
  persistence.saveBatchForm({
    smsProvider: 'smsbower',
    service: 'wa', country: '62', priceKey: 'offer', quantity: 2, pin: '123456', proxy: '',
  })

  assert.equal(persistence.clearApiKey(), true)
  assert.equal(persistence.clearBatchForm(), true)
  assert.equal(storage.getItem(API_KEY_STORAGE_KEY), null)
  assert.equal(storage.getItem(BATCH_FORM_STORAGE_KEY), null)
  assert.equal(persistence.loadApiKey(), '')
  assert.deepEqual(persistence.loadBatchForm(), DEFAULT_BATCH_FORM)
})
