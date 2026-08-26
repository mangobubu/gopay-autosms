import assert from 'node:assert/strict'
import test from 'node:test'

import { ApiError, validateAccountLoginStatusesPayload } from './api.ts'

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
