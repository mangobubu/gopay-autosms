import assert from 'node:assert/strict'
import test from 'node:test'

import {
  ACTIVE_BATCH_STORAGE_KEY,
  API_KEY_STORAGE_KEY,
  BATCH_FORM_STORAGE_KEY,
  DEFAULT_BATCH_FORM,
  HERO_SMS_API_KEY_STORAGE_KEY,
  clearLegacyClientPersistence,
  normalizeBatchForm,
} from './persistence.ts'

class MemoryStorage {
  values = new Map()
  removeCalls = []

  removeItem(key) {
    this.removeCalls.push(key)
    this.values.delete(key)
  }
}

test('clears every legacy client value without touching unrelated browser data', () => {
  const storage = new MemoryStorage()
  for (const key of [
    API_KEY_STORAGE_KEY,
    HERO_SMS_API_KEY_STORAGE_KEY,
    BATCH_FORM_STORAGE_KEY,
    ACTIVE_BATCH_STORAGE_KEY,
    'unrelated',
  ]) storage.values.set(key, 'value')

  assert.equal(clearLegacyClientPersistence(storage), true)
  assert.deepEqual(storage.removeCalls, [
    API_KEY_STORAGE_KEY,
    HERO_SMS_API_KEY_STORAGE_KEY,
    BATCH_FORM_STORAGE_KEY,
    ACTIVE_BATCH_STORAGE_KEY,
  ])
  assert.deepEqual([...storage.values], [['unrelated', 'value']])
})

test('attempts all legacy removals when one browser operation throws', () => {
  const calls = []
  const storage = {
    removeItem(key) {
      calls.push(key)
      if (key === BATCH_FORM_STORAGE_KEY) throw new Error('blocked')
    },
  }

  assert.equal(clearLegacyClientPersistence(storage), false)
  assert.equal(calls.length, 4)
  assert.equal(calls.at(-1), ACTIVE_BATCH_STORAGE_KEY)
})

test('returns fresh form defaults for missing or malformed server drafts', () => {
  const first = normalizeBatchForm(undefined)
  const second = normalizeBatchForm({ quantity: 101, pin: '12345x' })

  assert.notEqual(first, second)
  assert.deepEqual(first, DEFAULT_BATCH_FORM)
  assert.deepEqual(second, DEFAULT_BATCH_FORM)
  first.service = 'changed'
  assert.deepEqual(second, DEFAULT_BATCH_FORM)
})

test('normalizes camel-case and snake-case server draft fields', () => {
  assert.deepEqual(normalizeBatchForm({
    sms_provider: 'smsbower',
    service: 'goPay',
    country: 'ID',
    price_key: 'offer-7',
    quantity: 12,
    pin: '123456',
    proxy: '127.0.0.1:8080',
    price_snapshot: {
      value: 'offer-7',
      provider: '42',
      price: 1.25,
      tier: 'Gold',
      tierDerived: true,
    },
  }), {
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
  })
})

test('removes unsupported provider metadata from a HeroSMS price snapshot', () => {
  assert.deepEqual(normalizeBatchForm({
    smsProvider: 'hero-sms',
    priceKey: 'hero-price',
    priceSnapshot: {
      value: 'hero-price',
      price: 1,
      provider: '17',
      tier: 'Gold',
      tierDerived: false,
    },
  }), {
    ...DEFAULT_BATCH_FORM,
    smsProvider: 'hero-sms',
    priceKey: 'hero-price',
    priceSnapshot: { value: 'hero-price', price: 1 },
  })
})
