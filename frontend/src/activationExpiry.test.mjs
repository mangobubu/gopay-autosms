import assert from 'node:assert/strict'
import test from 'node:test'

import { activationExpiryPresentation } from './activationExpiry.ts'

const now = Date.parse('2026-08-26T10:00:00.000Z')

test('shows cancelled instead of an expiration time for a cancelled activation', () => {
  assert.deepEqual(
    activationExpiryPresentation(
      'cancelled',
      '2026-08-26T10:01:00.000Z',
      '2026-08-26T10:00:30.000Z',
      now,
    ),
    { label: '已取消' },
  )
})

test('shows cancelled after a classified provider cancellation is finalized', () => {
  for (const status of ['duplicate', 'phone_in_use', 'login_code_timeout', 'pin_code_timeout', 'pin_required', 'unregistered']) {
    assert.deepEqual(
      activationExpiryPresentation(
        status,
        '2026-08-26T10:01:00.000Z',
        '2026-08-26T10:00:30.000Z',
        now,
      ),
      { label: '已取消' },
    )
  }
})

test('keeps the expiration time while provider cancellation is still pending', () => {
  for (const status of ['phone_in_use', 'login_code_timeout', 'pin_code_timeout']) {
    const presentation = activationExpiryPresentation(
      status,
      '2026-08-26T10:01:00.000Z',
      undefined,
      now,
    )

    assert.notEqual(presentation.label, '已取消')
    assert.equal(presentation.countdown, '01分00秒')
  }
})

test('shows settled after a blocked PIN submission is finalized', () => {
  assert.deepEqual(
    activationExpiryPresentation(
      'pin_submission_blocked',
      '2026-08-26T10:01:00.000Z',
      '2026-08-26T10:00:30.000Z',
      now,
    ),
    { label: '已结算' },
  )
})

test('keeps the expiration time while blocked PIN settlement is still pending', () => {
  const presentation = activationExpiryPresentation(
    'pin_submission_blocked',
    '2026-08-26T10:01:00.000Z',
    undefined,
    now,
  )

  assert.notEqual(presentation.label, '已结算')
  assert.equal(presentation.countdown, '01分00秒')
})

test('keeps the expiration time and countdown for an active activation', () => {
  const presentation = activationExpiryPresentation(
    'polling',
    '2026-08-26T10:01:00.000Z',
    undefined,
    now,
  )

  assert.notEqual(presentation.label, '已取消')
  assert.equal(presentation.countdown, '01分00秒')
})
