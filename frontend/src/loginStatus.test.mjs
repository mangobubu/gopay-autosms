import assert from 'node:assert/strict'
import test from 'node:test'

import {
  findAccountLoginStatusByPhone,
  indexAccountLoginStatusesByPhone,
  phoneLookupKey,
} from './loginStatus.ts'

function account(id, phone, status = 'valid') {
  return {
    id,
    phone,
    status,
    refreshed: false,
    raw: {},
  }
}

test('matches GoPay login statuses to activations using normalized phone numbers', () => {
  const statuses = indexAccountLoginStatusesByPhone([
    account('account-2', '+62 (813) 4567 8901', 'invalid'),
    account('account-1', '+62 812-3456-7890'),
  ])

  assert.equal(findAccountLoginStatusByPhone(statuses, '+6281234567890')?.id, 'account-1')
  assert.equal(findAccountLoginStatusByPhone(statuses, '62 813 4567 8901')?.status, 'invalid')
})

test('does not index or match an account without the exact usable phone number', () => {
  const statuses = indexAccountLoginStatusesByPhone([
    account('account-1', '+62 812 3456 7890'),
    account('missing-phone', '未提供号码', 'unknown'),
  ])

  assert.equal(statuses.size, 1)
  assert.equal(phoneLookupKey('—'), '')
  assert.equal(findAccountLoginStatusByPhone(statuses, '+62 812 3456 7891'), undefined)
  assert.equal(findAccountLoginStatusByPhone(statuses, '—'), undefined)
})
