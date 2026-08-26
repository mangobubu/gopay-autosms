import assert from 'node:assert/strict'
import test from 'node:test'

import { findRefreshedPrice } from './priceOptions.ts'

function offer(value, provider, price, tier = undefined, stock = 1) {
  return {
    key: `${value}-${provider}`,
    value,
    label: value,
    provider,
    price,
    tier,
    stock,
    raw: {},
  }
}

test('keeps the exact refreshed offer when similar offers also exist', () => {
  const previous = offer('offer-b', '42', 1.25, 'Gold')
  const similar = offer('offer-a', '42', 1.25, 'Gold')
  const exact = offer('offer-b', '42', 1.25, 'Gold')

  assert.equal(findRefreshedPrice(previous, [similar, exact]), exact)
})

test('uses a unique compatible offer when its generated value changes', () => {
  const previous = offer('old-id', '42', 1.25, 'Silver')
  const refreshed = offer('new-id', '42', 1.25, 'Silver')

  assert.equal(findRefreshedPrice(previous, [refreshed]), refreshed)
})

test('clears ambiguous, sold-out, or unpriced refreshed selections', () => {
  const previous = offer('old-id', '42', 1.25, 'Bronze')
  const first = offer('first', '42', 1.25, 'Bronze')
  const second = offer('second', '42', 1.25, 'Bronze')

  assert.equal(findRefreshedPrice(previous, [first, second]), undefined)
  assert.equal(findRefreshedPrice(previous, [offer('old-id', '42', 1.25, 'Bronze', 0)]), undefined)
  assert.equal(findRefreshedPrice(previous, [offer('old-id', '42', undefined, 'Bronze')]), undefined)
})
