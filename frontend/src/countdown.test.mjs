import assert from 'node:assert/strict'
import test from 'node:test'

import { formatCountdown } from './normalizers.ts'

const now = Date.parse('2026-08-26T10:00:00.000Z')

test('formats the remaining duration as padded minutes and seconds', () => {
  assert.equal(formatCountdown('2026-08-26T10:00:59.000Z', now), '00分59秒')
  assert.equal(formatCountdown('2026-08-26T10:01:00.000Z', now), '01分00秒')
  assert.equal(formatCountdown('2026-08-26T18:01:05+08:00', now), '01分05秒')
})

test('rounds a partial remaining second up so the countdown does not expire early', () => {
  assert.equal(formatCountdown('2026-08-26T10:01:00.001Z', now), '01分01秒')
})

test('clamps expired times and ignores missing or invalid values', () => {
  assert.equal(formatCountdown('2026-08-26T10:00:00.000Z', now), '00分00秒')
  assert.equal(formatCountdown('2026-08-26T09:59:00.000Z', now), '00分00秒')
  assert.equal(formatCountdown(undefined, now), '—')
  assert.equal(formatCountdown('not-a-date', now), '—')
})
