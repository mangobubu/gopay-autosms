import assert from 'node:assert/strict'
import test from 'node:test'

import {
  accountLoginStatus,
  activationStatus,
  batchStatus,
  normalizeAccountLoginStatuses,
  normalizeActivation,
  normalizeBatch,
  normalizePrices,
} from './normalizers.ts'

test('maps stopped batches to the stopped label and an inactive state', () => {
  assert.deepEqual(batchStatus('cancelled'), {
    label: '已停止',
    type: 'info',
    active: false,
  })
})

test('normalizes purchased numbers independently from successful numbers', () => {
  const batch = normalizeBatch({
    batch: {
      id: 42,
      quantity: 3,
      purchased_count: 3,
      fulfilled_count: 1,
      inflight_count: 1,
      failed_count: 1,
    },
  })

  assert.equal(batch.total, 3)
  assert.equal(batch.purchased, 3)
  assert.equal(batch.successful, 1)
  assert.equal(batch.inflight, 1)
  assert.equal(batch.failed, 1)
})

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

test('maps a login code timeout to a terminal danger status', () => {
  assert.deepEqual(activationStatus('login_code_timeout'), {
    label: '等待验证码超时',
    type: 'danger',
    active: false,
  })
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
  assert.deepEqual(offers.map(({ tierDerived }) => tierDerived), [false, false, false])
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
  assert.equal(unknown.tierDerived, undefined)
  assert.doesNotMatch(unknown.label, /Platinum/)
})

test('shows explicit GoPay login failures as login failed', () => {
  assert.deepEqual(activationStatus('login_failed'), {
    label: '登录失败',
    type: 'danger',
    active: false,
  })
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

test('derives tiers for official getPricesV3 offers without rank metadata', () => {
  const offers = normalizePrices({
    prices: [
      { country: 6, service: 'ni', providerId: 3395, price: '0.004', count: 72 },
      { country: 6, service: 'ni', providerId: 3417, price: '0.004', count: 10 },
      { country: 6, service: 'ni', providerId: 3327, price: '0.007', count: 115 },
      { country: 6, service: 'ni', providerId: 3237, price: '0.008', count: 84 },
      { country: 6, service: 'ni', providerId: 3347, price: '0.012', count: 236 },
      { country: 6, service: 'ni', providerId: 3029, price: '0.014', count: 291 },
    ],
  })

  assert.deepEqual(
    offers.map(({ tier }) => tier),
    ['Bronze', 'Bronze', 'Bronze', 'Silver', 'Silver', 'Gold'],
  )
  assert.ok(offers.every(({ tierDerived }) => tierDerived))
  assert.ok(offers.every(({ label, tier }) => label.includes(tier)))
})

test('assigns the same derived tier to offers with the same price', () => {
  const offers = normalizePrices({
    prices: [
      { providerId: 101, price: 1, count: 1 },
      { providerId: 102, price: '1.00', count: 2 },
      { providerId: 103, price: 2, count: 3 },
      { providerId: 104, price: 3, count: 4 },
    ],
  })

  assert.deepEqual(offers.map(({ tier }) => tier), ['Bronze', 'Bronze', 'Silver', 'Gold'])
})

test('uses endpoint tiers for one or two distinct available prices', () => {
  const singlePrice = normalizePrices({
    prices: [
      { providerId: 101, price: 1, count: 1 },
      { providerId: 102, price: 1, count: 2 },
    ],
  })
  const twoPrices = normalizePrices({
    prices: [
      { providerId: 201, price: 1, count: 1 },
      { providerId: 202, price: 2, count: 1 },
    ],
  })

  assert.deepEqual(singlePrice.map(({ tier }) => tier), ['Bronze', 'Bronze'])
  assert.deepEqual(twoPrices.map(({ tier }) => tier), ['Bronze', 'Gold'])
})

test('does not derive tiers for sold-out or unpriced offers', () => {
  const offers = normalizePrices({
    prices: [
      { providerId: 101, price: 1, count: 1 },
      { providerId: 102, price: 2, count: 0 },
      { providerId: 103, count: 3 },
    ],
  })

  assert.deepEqual(offers.map(({ tier }) => tier), ['Bronze', undefined, undefined])
})

test('does not treat negative stock as an available price tier', () => {
  const offers = normalizePrices({
    prices: [
      { providerId: 101, price: 1, count: -1 },
      { providerId: 102, price: 2, count: 1 },
    ],
  })

  assert.deepEqual(offers.map(({ tier }) => tier), [undefined, 'Bronze'])
  assert.equal(offers[0].tierDerived, undefined)
})
