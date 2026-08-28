import assert from 'node:assert/strict'
import test from 'node:test'

import { heroSmsApi } from './heroSmsApi.ts'

function jsonResponse(payload) {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

test('omits duration_hours for activation purchases and includes positive rental hours', async () => {
  const requests = []
  const originalFetch = globalThis.fetch
  globalThis.fetch = async (input, init) => {
    requests.push({ input: String(input), init })
    return jsonResponse({ tasks: [], server_now: '2026-08-28T10:00:00Z' })
  }

  try {
    await heroSmsApi.createTasks({
      service: 'wa',
      country: '6',
      verificationType: 'sms',
      durationHours: 0,
      quantity: 1,
    }, 'activation-attempt-1')
    await heroSmsApi.createTasks({
      service: 'wa',
      country: '6',
      verificationType: 'sms',
      durationHours: 72,
      quantity: 2,
    }, 'rental-attempt-1')
  } finally {
    globalThis.fetch = originalFetch
  }

  assert.equal(requests[0].input, '/api/hero-sms/tasks')
  assert.deepEqual(JSON.parse(requests[0].init.body), {
    service: 'wa',
    country: '6',
    verification_type: 'sms',
    quantity: 1,
  })
  assert.deepEqual(JSON.parse(requests[1].init.body), {
    service: 'wa',
    country: '6',
    verification_type: 'sms',
    duration_hours: 72,
    quantity: 2,
  })
  assert.equal(new Headers(requests[0].init.headers).get('Idempotency-Key'), 'activation-attempt-1')
  assert.equal(new Headers(requests[1].init.headers).get('Idempotency-Key'), 'rental-attempt-1')
})

test('requests subsequent task pages with the server cursor', async () => {
  const requests = []
  const originalFetch = globalThis.fetch
  globalThis.fetch = async (input) => {
    requests.push(String(input))
    return jsonResponse({ tasks: [], server_now: '2026-08-28T10:00:00Z' })
  }

  try {
    await heroSmsApi.getTasks()
    await heroSmsApi.getTasks('500')
  } finally {
    globalThis.fetch = originalFetch
  }

  assert.deepEqual(requests, [
    '/api/hero-sms/tasks?limit=500',
    '/api/hero-sms/tasks?limit=500&cursor=500',
  ])
})

test('requests exact catalog offers for rental and activation duration choices', async () => {
  const requests = []
  const originalFetch = globalThis.fetch
  globalThis.fetch = async (input) => {
    requests.push(String(input))
    return jsonResponse({
      services: [],
      countries: [],
      verification_types: [],
      durations: [],
      offers: [],
    })
  }

  try {
    await heroSmsApi.getCatalog({
      service: 'wa',
      country: '6',
      verificationType: 'sms',
      durationHours: 72,
    })
    await heroSmsApi.getCatalog({
      service: 'wa',
      country: '6',
      verificationType: 'sms',
      durationHours: 0,
    })
  } finally {
    globalThis.fetch = originalFetch
  }

  assert.deepEqual(requests, [
    '/api/hero-sms/catalog?service=wa&country=6&verification_type=sms&duration_hours=72',
    '/api/hero-sms/catalog?service=wa&country=6&verification_type=sms&duration_hours=0',
  ])
})
