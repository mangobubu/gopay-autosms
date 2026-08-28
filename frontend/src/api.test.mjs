import assert from 'node:assert/strict'
import test from 'node:test'

import { api, ApiError, validateAccountLoginStatusesPayload } from './api.ts'

test('stops a batch through the batch stop endpoint with POST', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
  })

  let request
  globalThis.fetch = async (input, init) => {
    request = { input, init }
    return new Response(JSON.stringify({ id: 'batch/42', status: 'cancelled' }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  const result = await api.stopBatch('batch/42')

  assert.equal(request.input, '/api/batches/batch%2F42/stop')
  assert.equal(request.init.method, 'POST')
  assert.equal(request.init.credentials, 'same-origin')
  assert.deepEqual(result, { id: 'batch/42', status: 'cancelled' })
})

test('lists batches through the batch collection endpoint', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
  })

  let request
  globalThis.fetch = async (input, init) => {
    request = { input, init }
    return new Response(JSON.stringify({ batches: [{ id: 'batch-7', status: 'running' }] }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  const result = await api.getBatches()

  assert.equal(request.input, '/api/batches')
  assert.equal(request.init.credentials, 'same-origin')
  assert.deepEqual(result, { batches: [{ id: 'batch-7', status: 'running' }] })
})

test('requests the full activation dashboard page for a batch', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
  })

  let request
  globalThis.fetch = async (input, init) => {
    request = { input, init }
    return new Response(JSON.stringify({ activations: [] }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  const result = await api.getActivations('batch/42')

  assert.equal(request.input, '/api/activations?batch_id=batch%2F42&limit=500')
  assert.equal(request.init.credentials, 'same-origin')
  assert.deepEqual(result, { activations: [] })
})

test('routes settings and catalog requests through the selected SMS provider', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
  })

  const requests = []
  globalThis.fetch = async (input, init) => {
    requests.push({ input, init })
    return new Response('{}', {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  await api.getSettings('hero-sms')
  await api.saveSettings({ apiKey: 'hero-key' }, 'hero-sms')
  await api.getServices('hero-sms')
  await api.getCountries('tg', 'hero-sms')
  await api.getPrices('tg', '6', 'hero-sms')

  assert.deepEqual(requests.map(({ input }) => input), [
    '/api/settings/hero-sms',
    '/api/settings/hero-sms',
    '/api/catalog/services?sms_provider=hero-sms',
    '/api/catalog/countries?service=tg&sms_provider=hero-sms',
    '/api/catalog/prices?service=tg&country=6&sms_provider=hero-sms',
  ])
  assert.equal(requests[1].init.method, 'PUT')
  assert.deepEqual(JSON.parse(requests[1].init.body), { api_key: 'hero-key' })
  assert.equal(requests[4].init.cache, 'no-store')
})

test('loads and saves the complete batch draft through the server settings endpoint', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
  })

  const requests = []
  globalThis.fetch = async (input, init) => {
    requests.push({ input, init })
    return new Response('{}', {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  await api.getBatchDraft()
  await api.saveBatchDraft({
    smsProvider: 'hero-sms',
    service: 'tg',
    country: '6',
    priceKey: 'offer-7',
    quantity: 3,
    pin: '123',
    proxy: 'socks5://user:pass@proxy.example:1080',
    priceSnapshot: { value: 'offer-7', price: 1.25 },
  })

  assert.deepEqual(requests.map(({ input }) => input), [
    '/api/settings/batch-draft',
    '/api/settings/batch-draft',
  ])
  assert.equal(requests[0].init.cache, 'no-store')
  assert.equal(requests[1].init.method, 'PUT')
  assert.deepEqual(JSON.parse(requests[1].init.body), {
    sms_provider: 'hero-sms',
    service: 'tg',
    country: '6',
    price_key: 'offer-7',
    quantity: 3,
    pin: '123',
    proxy: 'socks5://user:pass@proxy.example:1080',
    price_snapshot: { value: 'offer-7', price: 1.25 },
  })
})

test('sends the selected SMS provider without inventing a HeroSMS currency', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
  })

  let request
  globalThis.fetch = async (input, init) => {
    request = { input, init }
    return new Response(JSON.stringify({ id: 'batch-hero' }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  await api.createBatch({
    sms_provider: 'hero-sms',
    service: 'tg',
    country: '6',
    quantity: 1,
    pin: '123456',
  })

  assert.equal(request.input, '/api/batches')
  assert.deepEqual(JSON.parse(request.init.body), {
    sms_provider: 'hero-sms',
    service: 'tg',
    country: '6',
    quantity: 1,
    pin: '123456',
  })
})

test('defaults legacy batch calls to SMSBower', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
  })

  let body
  globalThis.fetch = async (_input, init) => {
    body = JSON.parse(init.body)
    return new Response('{}', {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
  }

  await api.createBatch({
    service: 'tg',
    country: '6',
    quantity: 1,
    pin: '123456',
  })

  assert.equal(body.sms_provider, 'smsbower')
})

test('accepts an explicit account login status collection, including an empty one', () => {
  const payload = { accounts: [] }
  assert.equal(validateAccountLoginStatusesPayload(payload), payload)
})

test('accepts the Go API account status shape', () => {
  const payload = {
    accounts: [
      {
        id: 7,
        phone_number: '+6281234567890',
        login_status: 'valid',
        status: 'valid',
        valid: true,
        checked_at: '2026-08-26T01:02:03Z',
        refreshed: false,
      },
    ],
  }
  assert.equal(validateAccountLoginStatusesPayload(payload), payload)
})

test('rejects malformed successful login status responses so old UI state is retained', () => {
  for (const payload of [
    null,
    {},
    { accounts: null },
    { data: { accounts: [] } },
    { accounts: [null, 'bad'] },
    { accounts: [{ phone_number: '+62812' }] },
    { accounts: [{ id: '' }] },
    { accounts: [{ id: 0, phone_number: '+62812', login_status: 'unknown' }] },
    { accounts: [{ id: 1, phone_number: {}, login_status: 'unknown' }] },
    { accounts: [{ id: 1, phone_number: '+62812', login_status: 'bogus' }] },
    { accounts: [{ id: 1, phone_number: '+62812', login_status: 'valid', valid: false }] },
    { accounts: [{ id: 1, phone_number: '+62812', login_status: 'unknown', checked_at: [] }] },
    { accounts: [
      { id: 1, phone_number: '+62812', login_status: 'valid' },
      { id: 1, phone_number: '+62813', login_status: 'invalid' },
    ] },
  ]) {
    assert.throws(
      () => validateAccountLoginStatusesPayload(payload),
      (error) => error instanceof ApiError && error.message === 'GoPay 登录状态响应格式异常',
    )
  }
})
