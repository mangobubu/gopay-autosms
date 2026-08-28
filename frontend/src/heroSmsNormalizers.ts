import type {
  HeroSmsCatalog,
  HeroSmsCatalogOption,
  HeroSmsDurationOption,
  HeroSmsMessage,
  HeroSmsNumberTask,
  HeroSmsOffer,
  HeroSmsProductKind,
  HeroSmsRecord,
  HeroSmsTaskCapabilities,
  HeroSmsTaskEnvelope,
} from './heroSmsTypes'

function isRecord(value: unknown): value is HeroSmsRecord {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function pick(record: HeroSmsRecord, keys: readonly string[]): unknown {
  for (const key of keys) {
    if (record[key] !== undefined && record[key] !== null) return record[key]
  }
  return undefined
}

function text(value: unknown): string {
  if (typeof value === 'string') return value.trim()
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return ''
}

function numberValue(value: unknown): number | undefined {
  if (typeof value === 'number') return Number.isFinite(value) ? value : undefined
  if (typeof value !== 'string' || value.trim() === '') return undefined
  const parsed = Number(value.trim().replace(',', '.').replace(/[^\d.+-]/g, ''))
  return Number.isFinite(parsed) ? parsed : undefined
}

function integer(value: unknown): number | undefined {
  const parsed = numberValue(value)
  return parsed === undefined ? undefined : Math.max(0, Math.trunc(parsed))
}

function booleanValue(value: unknown): boolean | undefined {
  if (typeof value === 'boolean') return value
  if (typeof value === 'number') {
    if (value === 1) return true
    if (value === 0) return false
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase()
    if (['1', 'true', 'yes', 'available', 'enabled'].includes(normalized)) return true
    if (['0', 'false', 'no', 'unavailable', 'disabled'].includes(normalized)) return false
  }
  return undefined
}

function dateValue(value: unknown): string | undefined {
  const normalized = text(value)
  return normalized && Number.isFinite(Date.parse(normalized)) ? normalized : undefined
}

function unwrap(payload: unknown): unknown {
  if (!isRecord(payload)) return payload
  for (const key of ['data', 'result']) {
    const value = payload[key]
    if (value !== undefined && value !== null) return value
  }
  return payload
}

function arrayAt(record: HeroSmsRecord, keys: readonly string[]): unknown[] {
  for (const key of keys) {
    const value = unwrap(record[key])
    if (Array.isArray(value)) return value
  }
  return []
}

function option(
  value: unknown,
  index: number,
  valueKeys: readonly string[],
  labelKeys: readonly string[],
): HeroSmsCatalogOption | undefined {
  const raw: HeroSmsRecord = isRecord(value) ? value : { value }
  const optionValue = text(pick(raw, valueKeys))
  if (!optionValue) return undefined
  return {
    key: `${optionValue}-${index}`,
    value: optionValue,
    label: text(pick(raw, labelKeys)) || optionValue,
    description: text(pick(raw, ['description', 'subtitle', 'hint'])) || undefined,
    raw,
  }
}

function uniqueBy<T>(items: T[], keyOf: (item: T) => string): T[] {
  const indexes = new Map<string, number>()
  const result: T[] = []
  for (const item of items) {
    const key = keyOf(item)
    const index = indexes.get(key)
    if (index === undefined) {
      indexes.set(key, result.length)
      result.push(item)
    } else {
      result[index] = item
    }
  }
  return result
}

function productKind(record: HeroSmsRecord, durationSeconds?: number): HeroSmsProductKind {
  const normalized = text(pick(record, ['product_kind', 'productKind', 'kind', 'type'])).toLowerCase()
  if (normalized === 'rent' || normalized === 'rental') return 'rent'
  if (normalized === 'activation') return 'activation'
  return durationSeconds !== undefined && durationSeconds > 0 ? 'rent' : 'activation'
}

function durationFrom(record: HeroSmsRecord): { seconds?: number; hours?: number; label?: string } {
  const seconds = numberValue(pick(record, [
    'duration_seconds', 'durationSeconds', 'actual_duration_seconds', 'actualDurationSeconds',
  ]))
  const hours = numberValue(pick(record, ['duration_hours', 'durationHours', 'hours']))
  const normalizedSeconds = seconds !== undefined ? seconds : hours !== undefined ? hours * 3_600 : undefined
  return {
    seconds: normalizedSeconds,
    hours: normalizedSeconds === undefined ? undefined : normalizedSeconds / 3_600,
    label: text(pick(record, ['duration_label', 'durationLabel', 'label'])) || undefined,
  }
}

function durationLabel(seconds: number): string {
  if (seconds % 86_400 === 0) return `${seconds / 86_400} 天`
  if (seconds % 3_600 === 0) return `${seconds / 3_600} 小时`
  if (seconds % 60 === 0) return `${seconds / 60} 分钟`
  return `${seconds} 秒`
}

function normalizeOffer(value: unknown, index: number, inherited: HeroSmsRecord = {}): HeroSmsOffer | undefined {
  const own = isRecord(value) ? value : { price: value }
  const raw = { ...inherited, ...own }
  const duration = durationFrom(raw)
  const service = text(pick(raw, ['service', 'service_code', 'serviceCode']))
  const country = text(pick(raw, ['country', 'country_id', 'countryId', 'country_code', 'countryCode']))
  const verificationType = text(pick(raw, [
    'verification_type', 'verificationType', 'verification', 'channel', 'method',
  ])).toLowerCase() || undefined
  const price = numberValue(pick(raw, [
    'price', 'cost', 'amount', 'price_amount', 'priceAmount', 'activation_cost', 'activationCost',
  ]))
  const stock = integer(pick(raw, ['stock', 'available_count', 'availableCount', 'count', 'quantity']))
  const explicitAvailable = booleanValue(pick(raw, ['available', 'is_available', 'isAvailable']))
  const key = text(pick(raw, ['offer_key', 'offerKey', 'key', 'id', 'value']))
    || [service, country, verificationType, duration.seconds, price, index].join(':')
  return {
    key,
    service,
    serviceName: text(pick(raw, ['service_name', 'serviceName'])) || undefined,
    country,
    countryName: text(pick(raw, ['country_name', 'countryName'])) || undefined,
    verificationType,
    productKind: productKind(raw, duration.seconds),
    durationSeconds: duration.seconds,
    durationHours: duration.hours,
    durationLabel: duration.label || (duration.seconds !== undefined ? durationLabel(duration.seconds) : undefined),
    price,
    currency: text(pick(raw, ['currency', 'currency_code', 'currencyCode'])) || undefined,
    stock,
    available: explicitAvailable ?? (stock === undefined || stock > 0),
    refundableWindowSeconds: numberValue(pick(raw, [
      'refundable_window_seconds', 'refundableWindowSeconds', 'refund_window_seconds', 'refundWindowSeconds',
    ])),
    raw,
  }
}

function expandOffers(values: unknown[]): HeroSmsOffer[] {
  const result: HeroSmsOffer[] = []
  values.forEach((value, offerIndex) => {
    const raw = isRecord(value) ? value : { price: value }
    const durationOptions = arrayAt(raw, ['duration_options', 'durationOptions', 'durations'])
    if (durationOptions.length > 0) {
      durationOptions.forEach((duration, durationIndex) => {
        const normalized = normalizeOffer(duration, (offerIndex * 1_000) + durationIndex, raw)
        if (normalized) result.push(normalized)
      })
      return
    }
    const normalized = normalizeOffer(raw, offerIndex)
    if (normalized) result.push(normalized)
  })
  return result
}

function optionFromOffer(
  offer: HeroSmsOffer,
  kind: 'service' | 'country' | 'verification',
  index: number,
): HeroSmsCatalogOption | undefined {
  const value = kind === 'service' ? offer.service : kind === 'country' ? offer.country : offer.verificationType
  if (!value) return undefined
  const fallbackLabel = kind === 'service'
    ? offer.serviceName
    : kind === 'country'
      ? offer.countryName
      : verificationTypeLabel(value)
  return {
    key: `${value}-${index}`,
    value,
    label: fallbackLabel || value,
    raw: offer.raw,
  }
}

export function verificationTypeLabel(value: string): string {
  const normalized = value.trim().toLowerCase()
  if (normalized === 'sms') return '短信验证码'
  if (normalized === 'call') return '语音验证码'
  return value
}

export function normalizeHeroSmsCatalog(payload: unknown): HeroSmsCatalog {
  const value = unwrap(payload)
  const root: HeroSmsRecord = isRecord(value) ? value : {}
  const offers = expandOffers(Array.isArray(value) ? value : arrayAt(root, ['offers', 'prices', 'items']))
  const services = arrayAt(root, ['services']).map((item, index) => option(
    item,
    index,
    ['code', 'service', 'id', 'value'],
    ['name', 'label', 'title', 'service_name', 'serviceName'],
  )).filter((item): item is HeroSmsCatalogOption => Boolean(item))
  const countries = arrayAt(root, ['countries']).map((item, index) => option(
    item,
    index,
    ['id', 'country', 'value', 'code'],
    ['name', 'label', 'title', 'country_name', 'countryName', 'english'],
  )).filter((item): item is HeroSmsCatalogOption => Boolean(item))
  const verificationTypes = arrayAt(root, [
    'verification_types', 'verificationTypes', 'verification_methods', 'verificationMethods',
  ]).map((item, index) => option(
    item,
    index,
    ['value', 'code', 'type', 'id'],
    ['label', 'name', 'title'],
  )).filter((item): item is HeroSmsCatalogOption => Boolean(item))
    .map((item) => ({ ...item, label: verificationTypeLabel(item.value) }))

  const offerServices = offers.map((item, index) => optionFromOffer(item, 'service', index))
    .filter((item): item is HeroSmsCatalogOption => Boolean(item))
  const offerCountries = offers.map((item, index) => optionFromOffer(item, 'country', index))
    .filter((item): item is HeroSmsCatalogOption => Boolean(item))
  const offerVerificationTypes = offers.map((item, index) => optionFromOffer(item, 'verification', index))
    .filter((item): item is HeroSmsCatalogOption => Boolean(item))
  const explicitDurations = arrayAt(root, ['durations', 'duration_options', 'durationOptions'])
    .map((item, index): HeroSmsDurationOption | undefined => {
      const raw = isRecord(item) ? item : { duration_hours: item }
      const duration = durationFrom(raw)
      if (duration.seconds === undefined || duration.seconds <= 0) return undefined
      return {
        key: `${duration.seconds}-${index}`,
        seconds: duration.seconds,
        hours: duration.seconds / 3_600,
        label: duration.label || durationLabel(duration.seconds),
      }
    })
    .filter((item): item is HeroSmsDurationOption => Boolean(item))
  const offerDurations = offers
    .filter((item) => item.durationSeconds !== undefined && item.durationSeconds > 0)
    .map((item, index) => ({
      key: `${item.durationSeconds}-${index}`,
      seconds: item.durationSeconds as number,
      hours: (item.durationSeconds as number) / 3_600,
      label: item.durationLabel || durationLabel(item.durationSeconds as number),
    }))

  return {
    services: uniqueBy([...services, ...offerServices], (item) => item.value),
    countries: uniqueBy([...countries, ...offerCountries], (item) => item.value),
    verificationTypes: uniqueBy([...verificationTypes, ...offerVerificationTypes], (item) => item.value),
    durations: uniqueBy([...explicitDurations, ...offerDurations], (item) => String(item.seconds))
      .sort((left, right) => left.seconds - right.seconds),
    offers: uniqueBy(offers, (item) => item.key),
    message: text(pick(root, ['message', 'detail', 'error'])) || undefined,
  }
}

export function mergeHeroSmsCatalog(current: HeroSmsCatalog, incoming: HeroSmsCatalog): HeroSmsCatalog {
  return {
    services: uniqueBy([...current.services, ...incoming.services], (item) => item.value),
    countries: uniqueBy([...current.countries, ...incoming.countries], (item) => item.value),
    verificationTypes: uniqueBy([...current.verificationTypes, ...incoming.verificationTypes], (item) => item.value),
    durations: uniqueBy([...current.durations, ...incoming.durations], (item) => String(item.seconds))
      .sort((left, right) => left.seconds - right.seconds),
    offers: uniqueBy([...current.offers, ...incoming.offers], (item) => item.key),
    message: incoming.message ?? current.message,
  }
}

export interface HeroSmsDurationChoice {
  key: string
  hours: number
  label: string
}

export function heroSmsDurationChoices(
  offers: HeroSmsOffer[],
  fallback: HeroSmsDurationOption[] = [],
): HeroSmsDurationChoice[] {
  const rentOptions = offers
    .filter((offer) => offer.durationHours !== undefined && offer.durationHours > 0)
    .map((offer, index) => ({
      key: `${offer.durationHours}-${index}`,
      hours: offer.durationHours as number,
      label: offer.durationLabel || `${offer.durationHours} 小时`,
    }))
  const hasActivation = offers.some((offer) => (
    offer.productKind === 'activation' || offer.durationHours === undefined || offer.durationHours === 0
  ))
  if (rentOptions.length === 0 && !hasActivation) {
    return fallback.map((item) => ({ key: item.key, hours: item.hours, label: item.label }))
  }

  const source: HeroSmsDurationChoice[] = hasActivation
    ? [{ key: 'activation', hours: 0, label: '单次接码（约 20 分钟）' }, ...rentOptions]
    : rentOptions
  const seen = new Set<number>()
  return source.filter((item) => {
    if (seen.has(item.hours)) return false
    seen.add(item.hours)
    return true
  }).sort((left, right) => left.hours - right.hours)
}

function messageRecord(value: unknown): HeroSmsRecord {
  if (isRecord(value)) return value
  return { code: value }
}

function normalizeMessages(record: HeroSmsRecord): HeroSmsMessage[] {
  const candidates = arrayAt(record, ['messages', 'sms', 'codes', 'verification_codes', 'verificationCodes'])
  const messages = candidates.map((value, index): HeroSmsMessage | undefined => {
    const raw = messageRecord(value)
    const code = text(pick(raw, ['code', 'verification_code', 'verificationCode', 'otp'])) || undefined
    const body = text(pick(raw, ['text', 'message', 'body', 'sms'])) || undefined
    const receivedAt = dateValue(pick(raw, [
      'provider_received_at', 'providerReceivedAt', 'received_at', 'receivedAt', 'created_at', 'createdAt', 'date',
    ]))
    if (!code && !body) return undefined
    const id = text(pick(raw, ['id', 'message_id', 'messageId', 'event_id', 'eventId']))
      || [receivedAt, code, body, index].join(':')
    return {
      id,
      code,
      text: body,
      from: text(pick(raw, ['phone_from', 'phoneFrom', 'from', 'sender'])) || undefined,
      source: text(pick(raw, ['source'])) || undefined,
      receivedAt,
      raw,
    }
  }).filter((item): item is HeroSmsMessage => Boolean(item))
  return uniqueBy(messages, (item) => item.id)
    .sort((left, right) => (Date.parse(left.receivedAt ?? '') || 0) - (Date.parse(right.receivedAt ?? '') || 0))
}

const TERMINAL_STATUSES = new Set([
  'stopped', 'stopped_unpurchased', 'refunded', 'settled', 'expired', 'provider_ended', 'failed', 'cancelled',
])

function capabilityValue(record: HeroSmsRecord, keys: readonly string[]): boolean | undefined {
  return booleanValue(pick(record, keys))
}

function normalizeCapabilities(
  record: HeroSmsRecord,
  status: string,
  hasPhone: boolean,
  refundable?: boolean,
): HeroSmsTaskCapabilities {
  const source = isRecord(record.capabilities) ? record.capabilities : record
  const terminal = TERMINAL_STATUSES.has(status)
  return {
    start: capabilityValue(source, ['start', 'can_start', 'canStart'])
      ?? ((status === 'stopped' || status === 'stopped_unpurchased') && !hasPhone),
    stop: capabilityValue(source, ['stop', 'can_stop', 'canStop']) ?? (!terminal && !hasPhone),
    settle: capabilityValue(source, ['settle', 'can_settle', 'canSettle']) ?? (!terminal && hasPhone),
    cancel: capabilityValue(source, ['cancel', 'refund', 'can_cancel', 'canCancel', 'can_refund', 'canRefund'])
      ?? (!terminal && hasPhone && refundable !== false),
  }
}

export function normalizeHeroSmsTask(payload: unknown, fallbackIndex = 0): HeroSmsNumberTask | undefined {
  const value = unwrap(payload)
  if (!isRecord(value)) return undefined
  const id = text(pick(value, ['id', 'task_id', 'taskId', 'number_task_id', 'numberTaskId']))
  if (!id) return undefined
  const status = text(pick(value, ['status', 'state'])).toLowerCase().replace(/[\s-]+/g, '_') || 'queued'
  const phone = text(pick(value, ['phone_number', 'phoneNumber', 'phone', 'number'])) || undefined
  const refundStatus = text(pick(value, ['refund_status', 'refundStatus'])).toLowerCase() || undefined
  let refundable = booleanValue(pick(value, ['refundable', 'can_refund', 'canRefund']))
  if (refundable === undefined && refundStatus) {
    refundable = refundStatus === 'eligible' || refundStatus === 'refundable'
  }
  const requestedDuration = durationFrom({
    duration_seconds: pick(value, ['requested_duration_seconds', 'requestedDurationSeconds', 'duration_seconds', 'durationSeconds']),
    duration_hours: pick(value, ['requested_duration_hours', 'requestedDurationHours', 'duration_hours', 'durationHours']),
  })
  const effectiveDuration = durationFrom({
    duration_seconds: pick(value, ['effective_duration_seconds', 'effectiveDurationSeconds', 'actual_duration_seconds', 'actualDurationSeconds']),
  })
  const messages = normalizeMessages(value)
  if (messages.length > 0) refundable = false
  const explicitRunning = booleanValue(pick(value, ['running', 'active', 'is_running', 'isRunning']))
  const running = explicitRunning ?? !TERMINAL_STATUSES.has(status)
  const rawPrice = pick(value, [
    'purchase_price_amount', 'purchasePriceAmount', 'activation_cost', 'activationCost', 'price', 'cost', 'amount',
  ])
  return {
    id: id || `task-${fallbackIndex}`,
    status,
    service: text(pick(value, ['service', 'service_code', 'serviceCode'])),
    serviceName: text(pick(value, ['service_name', 'serviceName'])) || undefined,
    country: text(pick(value, ['country', 'country_id', 'countryId', 'country_code', 'countryCode'])),
    countryName: text(pick(value, ['country_name', 'countryName'])) || undefined,
    verificationType: text(pick(value, ['verification_type', 'verificationType', 'verification', 'channel'])) || undefined,
    productKind: productKind(value, requestedDuration.seconds),
    requestedDurationSeconds: requestedDuration.seconds,
    effectiveDurationSeconds: effectiveDuration.seconds,
    providerActivationId: text(pick(value, [
      'provider_activation_id', 'providerActivationId', 'activation_id', 'activationId',
    ])) || undefined,
    phone,
    operator: text(pick(value, ['operator', 'provider', 'carrier'])) || undefined,
    price: numberValue(rawPrice),
    currency: text(pick(value, ['currency', 'currency_code', 'currencyCode'])) || undefined,
    purchasedAt: dateValue(pick(value, ['purchased_at', 'purchasedAt', 'activated_at', 'activatedAt'])),
    expiresAt: dateValue(pick(value, ['expires_at', 'expiresAt', 'valid_until', 'validUntil'])),
    refundableUntil: dateValue(pick(value, [
      'refundable_until', 'refundableUntil', 'refund_deadline', 'refundDeadline',
    ])),
    refundStatus,
    refundable,
    nextAttemptAt: dateValue(pick(value, [
      'next_attempt_at', 'nextAttemptAt', 'next_run_at', 'nextRunAt', 'retry_at', 'retryAt',
    ])),
    retryCount: integer(pick(value, ['retry_count', 'retryCount', 'attempts'])),
    error: text(pick(value, ['last_error', 'lastError', 'error', 'failure_reason', 'failureReason'])) || undefined,
    running,
    capabilities: normalizeCapabilities(value, status, Boolean(phone), refundable),
    messages,
    createdAt: dateValue(pick(value, ['created_at', 'createdAt'])),
    updatedAt: dateValue(pick(value, ['updated_at', 'updatedAt'])),
    finishedAt: dateValue(pick(value, ['finished_at', 'finishedAt', 'stopped_at', 'stoppedAt'])),
    raw: value,
  }
}

export function normalizeHeroSmsTaskEnvelope(payload: unknown): HeroSmsTaskEnvelope {
  const value = unwrap(payload)
  const root: HeroSmsRecord = isRecord(value) ? value : {}
  const nestedTask = isRecord(root.task) ? normalizeHeroSmsTask(root.task) : undefined
  const directTask = nestedTask ?? normalizeHeroSmsTask(value)
  const candidates = Array.isArray(value)
    ? value
    : arrayAt(root, ['tasks', 'number_tasks', 'numberTasks', 'items'])
  const tasks = candidates.length > 0
    ? candidates.map(normalizeHeroSmsTask).filter((item): item is HeroSmsNumberTask => Boolean(item))
    : directTask ? [directTask] : []
  return {
    tasks: uniqueBy(tasks, (item) => item.id),
    serverNow: dateValue(pick(root, ['server_now', 'serverNow', 'now'])),
    nextCursor: text(pick(root, ['next_cursor', 'nextCursor'])) || undefined,
    message: text(pick(root, ['message', 'detail', 'error'])) || undefined,
  }
}

function mergeMessages(current: HeroSmsMessage[], incoming: HeroSmsMessage[]): HeroSmsMessage[] {
  return uniqueBy([...current, ...incoming], (item) => item.id)
    .sort((left, right) => (Date.parse(left.receivedAt ?? '') || 0) - (Date.parse(right.receivedAt ?? '') || 0))
}

export function mergeHeroSmsTasks(
  current: HeroSmsNumberTask[],
  incoming: HeroSmsNumberTask[],
): HeroSmsNumberTask[] {
  const result = current.map((item) => ({ ...item, messages: [...item.messages] }))
  const indexes = new Map(result.map((item, index) => [item.id, index]))
  for (const task of incoming) {
    const index = indexes.get(task.id)
    if (index === undefined) {
      indexes.set(task.id, result.length)
      result.push(task)
      continue
    }
    const existing = result[index]
    const existingUpdatedAt = Date.parse(existing.updatedAt ?? '')
    const incomingUpdatedAt = Date.parse(task.updatedAt ?? '')
    if (Number.isFinite(existingUpdatedAt)
      && Number.isFinite(incomingUpdatedAt)
      && incomingUpdatedAt < existingUpdatedAt) {
      result[index] = {
        ...existing,
        messages: mergeMessages(existing.messages, task.messages),
      }
      continue
    }
    result[index] = {
      ...existing,
      ...task,
      service: task.service || existing.service,
      serviceName: task.serviceName ?? existing.serviceName,
      country: task.country || existing.country,
      countryName: task.countryName ?? existing.countryName,
      verificationType: task.verificationType ?? existing.verificationType,
      requestedDurationSeconds: task.requestedDurationSeconds ?? existing.requestedDurationSeconds,
      effectiveDurationSeconds: task.effectiveDurationSeconds ?? existing.effectiveDurationSeconds,
      providerActivationId: task.providerActivationId ?? existing.providerActivationId,
      phone: task.phone ?? existing.phone,
      operator: task.operator ?? existing.operator,
      price: task.price ?? existing.price,
      currency: task.currency ?? existing.currency,
      purchasedAt: task.purchasedAt ?? existing.purchasedAt,
      expiresAt: task.expiresAt ?? existing.expiresAt,
      refundableUntil: task.refundableUntil ?? existing.refundableUntil,
      nextAttemptAt: task.nextAttemptAt ?? existing.nextAttemptAt,
      retryCount: task.retryCount ?? existing.retryCount,
      createdAt: task.createdAt ?? existing.createdAt,
      updatedAt: task.updatedAt ?? existing.updatedAt,
      finishedAt: task.finishedAt ?? existing.finishedAt,
      capabilities: { ...existing.capabilities, ...task.capabilities },
      messages: mergeMessages(existing.messages, task.messages),
    }
  }
  return result
}

export interface HeroSmsStatusMeta {
  label: string
  type: 'primary' | 'success' | 'warning' | 'danger' | 'info'
  active: boolean
}

export function heroSmsTaskStatus(status: string): HeroSmsStatusMeta {
  const normalized = status.trim().toLowerCase().replace(/[\s-]+/g, '_')
  const statuses: Record<string, HeroSmsStatusMeta> = {
    queued: { label: '等待开始', type: 'info', active: true },
    waiting_number: { label: '暂无号码，持续购买中', type: 'warning', active: true },
    purchasing: { label: '正在购买号码', type: 'primary', active: true },
    acquiring: { label: '正在购买号码', type: 'primary', active: true },
    waiting_inventory: { label: '暂无号码，持续购买中', type: 'warning', active: true },
    waiting_stock: { label: '暂无号码，持续购买中', type: 'warning', active: true },
    purchase_unknown: { label: '购买结果待核对', type: 'warning', active: true },
    receiving: { label: '接收验证码中', type: 'success', active: true },
    active: { label: '接收验证码中', type: 'success', active: true },
    settlement_requested: { label: '等待结算', type: 'warning', active: true },
    settling: { label: '正在结算', type: 'warning', active: true },
    expiring: { label: '正在到期处理', type: 'warning', active: true },
    stopped_unpurchased: { label: '已停止购买', type: 'info', active: false },
    stopped: { label: '已停止', type: 'info', active: false },
    refunded: { label: '已退款', type: 'success', active: false },
    settled: { label: '已结算', type: 'success', active: false },
    expired: { label: '号码已过期', type: 'info', active: false },
    provider_ended: { label: '服务已结束', type: 'info', active: false },
    failed: { label: '任务失败', type: 'danger', active: false },
    cancelled: { label: '已取消', type: 'info', active: false },
  }
  return statuses[normalized] ?? {
    label: normalized ? normalized.replace(/_/g, ' ') : '状态未知',
    type: 'info',
    active: !TERMINAL_STATUSES.has(normalized),
  }
}
