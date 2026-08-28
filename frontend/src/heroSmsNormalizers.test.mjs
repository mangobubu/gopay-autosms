import assert from 'node:assert/strict'
import test from 'node:test'

import {
  heroSmsDurationChoices,
  heroSmsTaskStatus,
  mergeHeroSmsTasks,
  normalizeHeroSmsCatalog,
  normalizeHeroSmsTask,
  normalizeHeroSmsTaskEnvelope,
} from './heroSmsNormalizers.ts'

test('expands selectable durations while retaining activation offers and verification types', () => {
  const catalog = normalizeHeroSmsCatalog({
    data: {
      verification_types: [
        { value: 'sms', label: 'SMS' },
        { value: 'call', label: 'Call' },
      ],
      offers: [
        {
          id: 'whatsapp-id-rental',
          service: 'wa',
          service_name: 'WhatsApp',
          country_id: '6',
          country_name: '印度尼西亚',
          verification_type: 'sms',
          currency: 'USD',
          duration_options: [
            { id: 'wa-2h', duration_hours: 2, price: '0.25', stock: 4 },
            { id: 'wa-3d', duration_seconds: 259_200, price: '0.6125', stock: 8 },
          ],
        },
        {
          id: 'whatsapp-call-activation',
          service: 'wa',
          country_id: '6',
          verification_type: 'call',
          duration_hours: 0,
          price: 0.18,
          stock: 3,
        },
      ],
    },
  })

  assert.deepEqual(
    catalog.durations.map(({ seconds, hours, label }) => ({ seconds, hours, label })),
    [
      { seconds: 7_200, hours: 2, label: '2 小时' },
      { seconds: 259_200, hours: 72, label: '3 天' },
    ],
  )
  assert.deepEqual(
    catalog.verificationTypes.map(({ value, label }) => [value, label]),
    [
      ['sms', '短信验证码'],
      ['call', '语音验证码'],
    ],
  )
  assert.deepEqual(catalog.offers.map(({ key, productKind }) => [key, productKind]), [
    ['wa-2h', 'rent'],
    ['wa-3d', 'rent'],
    ['whatsapp-call-activation', 'activation'],
  ])
  assert.deepEqual(
    heroSmsDurationChoices(catalog.offers).map(({ hours, label }) => [hours, label]),
    [
      [0, '单次接码（约 20 分钟）'],
      [2, '2 小时'],
      [72, '3 天'],
    ],
  )
})

test('normalizes snake_case task fields, capabilities, and webhook messages', () => {
  const task = normalizeHeroSmsTask({
    data: {
      task_id: 42,
      status: 'waiting-inventory',
      service_code: 'wa',
      service_name: 'WhatsApp',
      country_id: 6,
      country_name: '印度尼西亚',
      verification_type: 'sms',
      requested_duration_hours: 72,
      effective_duration_seconds: 259_200,
      provider_activation_id: 'hero-9001',
      phone_number: '+628123456789',
      activation_cost: '0.6125',
      currency_code: 'USD',
      purchased_at: '2026-08-26T10:00:00Z',
      valid_until: '2026-08-29T10:00:00Z',
      refund_deadline: '2026-08-26T10:20:00Z',
      refund_status: 'eligible',
      next_run_at: '2026-08-26T10:00:05Z',
      retry_count: '3',
      is_running: true,
      capabilities: {
        can_start: false,
        can_stop: true,
        can_settle: true,
        can_refund: true,
      },
      verification_codes: [{
        event_id: 'evt-1',
        verification_code: '482901',
        message: 'Your code is 482901',
        phone_from: 'WhatsApp',
        source: 'webhook',
        provider_received_at: '2026-08-26T10:01:00Z',
      }],
      created_at: '2026-08-26T09:59:00Z',
      updated_at: '2026-08-26T10:01:01Z',
    },
  })

  assert.ok(task)
  assert.equal(task.id, '42')
  assert.equal(task.status, 'waiting_inventory')
  assert.equal(task.service, 'wa')
  assert.equal(task.country, '6')
  assert.equal(task.verificationType, 'sms')
  assert.equal(task.productKind, 'rent')
  assert.equal(task.requestedDurationSeconds, 259_200)
  assert.equal(task.effectiveDurationSeconds, 259_200)
  assert.equal(task.providerActivationId, 'hero-9001')
  assert.equal(task.phone, '+628123456789')
  assert.equal(task.price, 0.6125)
  assert.equal(task.expiresAt, '2026-08-29T10:00:00Z')
  assert.equal(task.refundableUntil, '2026-08-26T10:20:00Z')
  assert.equal(task.retryCount, 3)
  assert.equal(task.running, true)
  assert.deepEqual(task.capabilities, {
    start: false,
    stop: true,
    settle: true,
    cancel: true,
  })
  assert.deepEqual(task.messages.map(({ id, code, source }) => ({ id, code, source })), [
    { id: 'evt-1', code: '482901', source: 'webhook' },
  ])
  assert.equal(task.refundable, false, '收到验证码后应当立即失去退款资格')
})

test('maps the durable backend statuses and refund vocabulary', () => {
  const waiting = normalizeHeroSmsTask({
    id: 1,
    status: 'waiting_number',
    refund_status: 'unavailable',
  })
  const active = normalizeHeroSmsTask({
    id: 2,
    status: 'active',
    phone_number: '+62812',
    refund_status: 'refundable',
  })
  const stopped = normalizeHeroSmsTask({ id: 3, status: 'stopped' })

  assert.ok(waiting)
  assert.ok(active)
  assert.ok(stopped)
  assert.equal(waiting.running, true)
  assert.equal(waiting.capabilities.stop, true)
  assert.equal(active.running, true)
  assert.equal(active.refundable, true)
  assert.equal(stopped.running, false)
  assert.equal(stopped.capabilities.start, true)
  assert.deepEqual(heroSmsTaskStatus('waiting_number'), {
    label: '暂无号码，持续购买中',
    type: 'warning',
    active: true,
  })
  assert.deepEqual(heroSmsTaskStatus('active'), {
    label: '接收验证码中',
    type: 'success',
    active: true,
  })
  assert.deepEqual(heroSmsTaskStatus('stopped'), {
    label: '已停止',
    type: 'info',
    active: false,
  })
  assert.deepEqual(heroSmsTaskStatus('purchasing'), {
    label: '正在购买号码',
    type: 'primary',
    active: true,
  })
})

test('merges by task ID without dropping older tasks and deduplicates message IDs', () => {
  const current = normalizeHeroSmsTaskEnvelope({
    tasks: [
      { id: 'older-task', status: 'receiving', service: 'tg', country: '1' },
      {
        id: 'same-task',
        status: 'receiving',
        service: 'wa',
        country: '6',
        messages: [
          { id: 'sms-1', code: '111111', received_at: '2026-08-26T10:01:00Z' },
        ],
      },
    ],
  }).tasks
  const incoming = normalizeHeroSmsTaskEnvelope({
    tasks: [
      {
        id: 'same-task',
        status: 'receiving',
        service: 'wa',
        country: '6',
        messages: [
          { id: 'sms-1', code: '111111', received_at: '2026-08-26T10:01:00Z' },
          { id: 'sms-2', code: '222222', received_at: '2026-08-26T10:02:00Z' },
        ],
      },
      { id: 'new-task', status: 'queued', service: 'wa', country: '6' },
    ],
  }).tasks

  const merged = mergeHeroSmsTasks(current, incoming)

  assert.deepEqual(merged.map(({ id }) => id), ['older-task', 'same-task', 'new-task'])
  assert.deepEqual(
    merged.find(({ id }) => id === 'same-task')?.messages.map(({ id, code }) => [id, code]),
    [
      ['sms-1', '111111'],
      ['sms-2', '222222'],
    ],
  )
})

test('does not let an older polling response roll a task state backward', () => {
  const current = normalizeHeroSmsTaskEnvelope({ tasks: [{
    id: 'task-1',
    status: 'active',
    phone_number: '+62812',
    updated_at: '2026-08-26T10:02:00Z',
  }] }).tasks
  const stale = normalizeHeroSmsTaskEnvelope({ tasks: [{
    id: 'task-1',
    status: 'waiting_number',
    updated_at: '2026-08-26T10:01:00Z',
    messages: [{ id: 'message-1', code: '112233' }],
  }] }).tasks

  const [merged] = mergeHeroSmsTasks(current, stale)
  assert.equal(merged.status, 'active')
  assert.equal(merged.phone, '+62812')
  assert.deepEqual(merged.messages.map(({ code }) => code), ['112233'])
})

test('marks zero-stock offers unavailable and keeps no-inventory tasks active', () => {
  const catalog = normalizeHeroSmsCatalog({
    offers: [{
      id: 'sold-out',
      service: 'wa',
      country: '6',
      verification_type: 'sms',
      stock: 0,
    }],
  })

  assert.equal(catalog.offers[0].available, false)
  assert.deepEqual(heroSmsTaskStatus('waiting_inventory'), {
    label: '暂无号码，持续购买中',
    type: 'warning',
    active: true,
  })
  assert.deepEqual(heroSmsTaskStatus('waiting-stock'), {
    label: '暂无号码，持续购买中',
    type: 'warning',
    active: true,
  })
})
