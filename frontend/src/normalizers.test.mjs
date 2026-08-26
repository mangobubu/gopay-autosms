import assert from 'node:assert/strict'
import test from 'node:test'

import {
  accountLoginStatus,
  activationStatus,
  normalizeAccountLoginStatuses,
  normalizeActivation,
  normalizePrices,
} from './normalizers.ts'

test('normalizes GoPay login status without treating unknown as expired', () => {
  const accounts = normalizeAccountLoginStatuses({
    accounts: [
      { id: 7, phone_number: '+62812', login_status: 'valid', checked_at: '2026-08-26T01:02:03Z' },
      { id: 8, phone: '+62813', status: 'expired', error: 'token rejected' },
      { id: 9, phone: '+62814', status: 'temporary_error' },
    ],
  })

  assert.deepEqual(accounts.map(({ status }) => status), ['valid', 'invalid', 'unknown'])
  assert.equal(accountLoginStatus(accounts[0].status).label, '登录有效')
  assert.equal(accountLoginStatus(accounts[1].status).label, '登录已失效')
  assert.equal(accountLoginStatus(accounts[2].status).label, '状态未知')
})

test('shows each durable PIN workflow status without inferring from historical codes', () => {
	assert.equal(activationStatus('awaiting_pin_code').label, '等待改 PIN 验证码')
  assert.equal(activationStatus('setting_pin').label, '正在设置 PIN')
  assert.equal(activationStatus('pin_changed').label, '改 PIN 成功')
  assert.equal(activationStatus('awaiting_subsequent_code').label, '等待后续验证码')
})

test('keeps activation errors available while PIN setup is being processed', () => {
  const activation = normalizeActivation({
    id: 'activation-1',
    status: 'awaiting_pin_code',
    pin_code: '9949',
    last_error: 'PIN 设置失败',
  })

  assert.equal(activation.pinCode, '9949')
  assert.equal(activation.error, 'PIN 设置失败')
})

test('sorts priced offers from low to high and keeps unpriced offers last', () => {
  const offers = normalizePrices({
    prices: [
      { id: 'expensive', price: '12.5', provider: '103' },
      { id: 'unpriced', provider: '104' },
      { id: 'cheapest', price: '0.85', provider: '101' },
      { id: 'middle', price: 3, provider: '102' },
    ],
  })

  assert.deepEqual(
    offers.map(({ value, price }) => [value, price]),
    [
      ['cheapest', 0.85],
      ['middle', 3],
      ['expensive', 12.5],
      ['unpriced', undefined],
    ],
  )
})

test('keeps the API order stable when two offers have the same price', () => {
  const offers = normalizePrices({
    offers: [
      { id: 'first', price: 2, provider: '201' },
      { id: 'second', price: '2.00', provider: '202' },
      { id: 'third', price: 2, provider: '203' },
    ],
  })

  assert.deepEqual(offers.map(({ value }) => value), ['first', 'second', 'third'])
})

test('normalizes and displays Bronze, Silver, and Gold provider tiers', () => {
  const offers = normalizePrices({
    prices: [
      { id: 'bronze', price: 1, rank: ' bronze ' },
      { id: 'silver', price: 2, provider_rank: 'SILVER' },
      { id: 'gold', price: 3, providerRank: 'Gold' },
    ],
  })

  assert.deepEqual(offers.map(({ tier }) => tier), ['Bronze', 'Silver', 'Gold'])
  assert.deepEqual(
    offers.map(({ label }) => label.includes('Bronze') || label.includes('Silver') || label.includes('Gold')),
    [true, true, true],
  )
})

test('accepts supported tier aliases and ignores unknown values', () => {
  const aliases = [
    ['tier', 'Bronze', 'Bronze'],
    ['level', 'silver', 'Silver'],
    ['grade', 'GOLD', 'Gold'],
    ['quality', ' silver ', 'Silver'],
  ]

  for (const [field, value, expected] of aliases) {
    const [offer] = normalizePrices({ prices: [{ id: field, price: 1, [field]: value }] })
    assert.equal(offer.tier, expected, `${field} should normalize to ${expected}`)
    assert.match(offer.label, new RegExp(expected))
  }

  const [unknown] = normalizePrices({ prices: [{ id: 'unknown', price: 1, tier: 'Platinum' }] })
  assert.equal(unknown.tier, undefined)
  assert.doesNotMatch(unknown.label, /Platinum/)
})

test('accepts numeric and object-shaped provider ranks', () => {
  const offers = normalizePrices({
    prices: [
      { id: 'gold', price: 1, rank: 1 },
      { id: 'silver', price: 2, provider_rank: { id: '2' } },
      { id: 'bronze', price: 3, rank: { id: 3, description: 'bronze' } },
      { id: 'alias', price: 4, tier: 'Gold tier' },
      { id: 'fallback', price: 5, tier: '', rank: { id: 3, description: 'bronze' } },
      { id: 'null-fallback', price: 6, tier: null, provider_rank: 2 },
    ],
  })

  assert.deepEqual(
    offers.map(({ tier }) => tier),
    ['Gold', 'Silver', 'Bronze', 'Gold', 'Bronze', 'Silver'],
  )
})
