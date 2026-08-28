import assert from 'node:assert/strict'
import test from 'node:test'

import {
  formatHeroSmsDuration,
  heroSmsCountdown,
  heroSmsRefundCountdown,
} from './heroSmsCountdown.ts'

const now = Date.parse('2026-08-26T10:00:00.000Z')

test('formats validity periods longer than one day without losing the day component', () => {
  const threeDaysOneHourTwoMinutesThreeSeconds = (((3 * 24) + 1) * 60 * 60 * 1_000)
    + (2 * 60 * 1_000)
    + (3 * 1_000)

  assert.equal(formatHeroSmsDuration(threeDaysOneHourTwoMinutesThreeSeconds), '3天 01:02:03')
  assert.equal(
    heroSmsCountdown('2026-08-29T11:02:03.000Z', now).label,
    '3天 01:02:03',
  )
})

test('rounds partial seconds up and expires exactly at the validity boundary', () => {
  assert.deepEqual(heroSmsCountdown('2026-08-26T10:00:00.001Z', now), {
    expired: false,
    remainingMs: 1,
    label: '00:00:01',
  })
  assert.deepEqual(heroSmsCountdown('2026-08-26T10:00:00.000Z', now), {
    expired: true,
    remainingMs: 0,
    label: '00:00:00',
  })
  assert.deepEqual(heroSmsCountdown('invalid-date', now), {
    expired: false,
    label: '—',
  })
})

test('keeps refunds eligible immediately before the deadline and closes them at the boundary', () => {
  const task = {
    phone: '+628123456789',
    messages: [],
    refundable: true,
    refundStatus: 'eligible',
    refundableUntil: '2026-08-26T10:20:00.000Z',
  }

  assert.deepEqual(heroSmsRefundCountdown(task, Date.parse('2026-08-26T10:19:59.999Z')), {
    eligible: true,
    expired: false,
    remainingMs: 1,
    label: '00:00:01',
    reason: 'eligible',
  })
  assert.deepEqual(heroSmsRefundCountdown(task, Date.parse('2026-08-26T10:20:00.000Z')), {
    eligible: false,
    expired: true,
    remainingMs: 0,
    label: '已超过退款时限',
    reason: 'window_elapsed',
  })
})

test('does not start the refund timer before purchase and forfeits it after a message arrives', () => {
  assert.deepEqual(heroSmsRefundCountdown({
    messages: [],
    refundable: true,
    refundStatus: 'eligible',
    refundableUntil: '2026-08-26T10:20:00.000Z',
  }, now), {
    eligible: false,
    expired: false,
    label: '号码购买后开始',
    reason: 'waiting_purchase',
  })

  assert.deepEqual(heroSmsRefundCountdown({
    phone: '+628123456789',
    messages: [{ id: 'sms-1', code: '482901', raw: {} }],
    refundable: true,
    refundStatus: 'eligible',
    refundableUntil: '2026-08-26T10:20:00.000Z',
  }, now), {
    eligible: false,
    expired: false,
    label: '已收到验证码，不可退款',
    reason: 'message_received',
  })
})

test('presents the backend refund lifecycle explicitly', () => {
  const base = {
    phone: '+628123456789',
    messages: [],
    refundableUntil: '2026-08-26T10:20:00.000Z',
  }
  assert.equal(heroSmsRefundCountdown({ ...base, refundStatus: 'refundable' }, now).eligible, true)
  assert.equal(heroSmsRefundCountdown({ ...base, refundStatus: 'requested' }, now).label, '退款处理中')
  assert.equal(heroSmsRefundCountdown({ ...base, refundStatus: 'refunded' }, now).label, '已退款')
  assert.equal(heroSmsRefundCountdown({ ...base, refundStatus: 'unavailable' }, now).label, '不可退款')
  assert.equal(heroSmsRefundCountdown({ ...base, refundStatus: 'settled' }, now).label, '已结算，不可退款')

  const received = { ...base, messages: [{ id: 'message-1', code: '123456', raw: {} }] }
  assert.equal(heroSmsRefundCountdown({ ...received, refundStatus: 'requested' }, now).label, '退款处理中')
  assert.equal(heroSmsRefundCountdown({ ...received, refundStatus: 'refunded' }, now).label, '已退款')
  assert.equal(heroSmsRefundCountdown({ ...received, refundStatus: 'settled' }, now).label, '已结算，不可退款')
})

test('keeps a long rental refundable during its separate short refund window', () => {
  const presentation = heroSmsRefundCountdown({
    productKind: 'rent',
    requestedDurationSeconds: 3 * 24 * 60 * 60,
    phone: '+628123456789',
    messages: [],
    refundable: true,
    refundStatus: 'refundable',
    refundableUntil: '2026-08-26T10:20:00.000Z',
  }, now)

  assert.equal(presentation.eligible, true)
  assert.equal(presentation.label, '00:20:00')
})
