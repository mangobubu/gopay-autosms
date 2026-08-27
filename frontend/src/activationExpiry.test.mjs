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
  for (const status of ['duplicate', 'pin_required', 'unregistered']) {
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
  const presentation = activationExpiryPresentation(
    'duplicate',
    '2026-08-26T10:01:00.000Z',
    undefined,
    now,
  )

  assert.notEqual(presentation.label, '已取消')
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
