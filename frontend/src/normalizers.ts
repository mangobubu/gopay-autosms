import type {
  ActivationView,
  BatchView,
  CatalogOption,
  PriceOption,
  SMSBowerSettings,
  StatusMeta,
  UnknownRecord,
  VerificationCodeView,
} from './types'

function isRecord(value: unknown): value is UnknownRecord {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function pick(record: UnknownRecord, keys: string[]): unknown {
  for (const key of keys) {
    if (record[key] !== undefined && record[key] !== null) return record[key]
  }
  return undefined
}

function asText(value: unknown, fallback = ''): string {
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return fallback
}

function asNumber(value: unknown, fallback = 0): number {
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string') {
    const parsed = Number.parseFloat(value.replace(',', '.').replace(/[^\d.-]/g, ''))
    if (Number.isFinite(parsed)) return parsed
  }
  return fallback
}

function asBoolean(value: unknown): boolean {
  if (typeof value === 'boolean') return value
  if (typeof value === 'number') return value !== 0
  if (typeof value === 'string') return ['1', 'true', 'yes'].includes(value.toLowerCase())
  return false
}

export function unwrap(payload: unknown): unknown {
  if (!isRecord(payload)) return payload
  for (const key of ['data', 'result']) {
    const value = payload[key]
    if (value !== undefined && value !== null) return value
  }
  return payload
}

function collection(payload: unknown, keys: string[]): unknown {
  let value = unwrap(payload)
  if (isRecord(value)) {
    for (const key of keys) {
      if (value[key] !== undefined) {
        value = unwrap(value[key])
        break
      }
    }
  }
  return value
}

function entries(payload: unknown, keys: string[]): Array<[string, unknown]> {
  const value = collection(payload, keys)
  if (Array.isArray(value)) return value.map((item, index) => [String(index), item])
  if (isRecord(value)) return Object.entries(value)
  return []
}

function optionRecord(key: string, value: unknown): UnknownRecord {
  if (isRecord(value)) return value
  return { code: key, name: value }
}

function normalizeOptions(
  payload: unknown,
  collectionKeys: string[],
  valueKeys: string[],
  labelKeys: string[],
): CatalogOption[] {
  return entries(payload, collectionKeys)
    .map<CatalogOption | null>(([entryKey, value], index) => {
      const raw = optionRecord(entryKey, value)
      const optionValue = asText(pick(raw, valueKeys), Array.isArray(collection(payload, collectionKeys)) ? '' : entryKey)
      const label = asText(pick(raw, labelKeys), optionValue || entryKey)
      if (!optionValue) return null
      return {
        key: `${optionValue}-${index}`,
        value: optionValue,
        label,
        description: asText(pick(raw, ['description', 'label', 'title'])) || undefined,
        raw,
      }
    })
    .filter((item): item is CatalogOption => item !== null)
}

export function normalizeServices(payload: unknown): CatalogOption[] {
  return normalizeOptions(
    payload,
    ['services', 'items'],
    ['code', 'service', 'id', 'value'],
    ['name', 'label', 'title', 'service_name'],
  )
}

export function normalizeCountries(payload: unknown): CatalogOption[] {
  return normalizeOptions(
    payload,
    ['countries', 'items'],
    ['code', 'country', 'id', 'value'],
    ['name', 'label', 'title', 'country_name'],
  )
}

export function normalizePrices(payload: unknown): PriceOption[] {
  return entries(payload, ['prices', 'items', 'providers', 'offers'])
    .map(([entryKey, value], index) => {
      const raw = optionRecord(entryKey, value)
      const provider = asText(
        pick(raw, ['provider', 'provider_id', 'providerId', 'operator', 'vendor', 'supplier', 'name']),
      )
      const rawPrice = pick(raw, ['price', 'cost', 'amount', 'rate', 'value'])
      const price = rawPrice === undefined ? undefined : asNumber(rawPrice)
      const rawStock = pick(raw, ['stock', 'count', 'available', 'quantity'])
      const stock = rawStock === undefined ? undefined : asNumber(rawStock)
      const id = asText(
        pick(raw, ['id', 'code', 'key', 'provider_id', 'providerId']),
        `${provider}:${price ?? entryKey}`,
      )
      const parts = [price === undefined ? '价格待定' : `${formatNumber(price)} ₽`]
      if (provider) parts.push(provider)
      if (stock !== undefined) parts.push(`库存 ${formatNumber(stock)}`)
      return {
        key: `${id}-${index}`,
        value: id,
        label: parts.join(' · '),
        description: asText(pick(raw, ['description', 'label', 'title'])),
        price,
        provider,
        stock,
        raw,
      } satisfies PriceOption
    })
}

export function normalizeSettings(payload: unknown): SMSBowerSettings {
  const value = unwrap(payload)
  const record = isRecord(value) && isRecord(value.settings) ? value.settings : value
  if (!isRecord(record)) return { apiKey: '', configured: false }
  return {
    apiKey: asText(pick(record, ['api_key', 'apiKey', 'key'])),
    configured: asBoolean(pick(record, ['configured', 'is_configured', 'isConfigured'])),
  }
}

function batchRecord(payload: unknown): UnknownRecord {
  const value = unwrap(payload)
  if (!isRecord(value)) return {}
  if (isRecord(value.batch)) return value.batch
  return value
}

export function normalizeBatch(payload: unknown): BatchView {
  const record = batchRecord(payload)
  const total = asNumber(pick(record, ['total', 'quantity', 'target', 'requested_count']))
  const successful = asNumber(pick(record, [
    'successful',
    'success',
    'success_count',
    'valid_count',
    'fulfilled_count',
    'fulfilledCount',
  ]))
  const inflight = asNumber(pick(record, ['inflight', 'inflight_count', 'inflightCount']))
  const failed = asNumber(pick(record, ['failed', 'failed_count']))
  const completed = asNumber(
    pick(record, ['completed', 'processed', 'completed_count']),
    successful + failed,
  )
  return {
    id: asText(pick(record, ['id', 'batch_id', 'batchId'])),
    status: asText(pick(record, ['status', 'state']), 'pending').toLowerCase(),
    total,
    completed,
    successful,
    inflight,
    proxyAvailable: asNumber(pick(record, ['proxy_available', 'proxyAvailable'])),
    proxyTotal: asNumber(pick(record, ['proxy_total', 'proxyTotal'])),
    failed,
    message: asText(pick(record, [
      'message',
      'error',
      'detail',
      'failure_reason',
      'failureReason',
      'last_error',
      'lastError',
    ])),
    createdAt: asText(pick(record, ['created_at', 'createdAt'])) || undefined,
  }
}

function codeFrom(value: unknown): string {
  if (typeof value === 'string' || typeof value === 'number') return String(value)
  if (!isRecord(value)) return ''
  return asText(pick(value, ['code', 'otp', 'value', 'sms_code', 'smsCode']))
}

function normalizeCodeList(value: unknown, loginCode: string, pinCode: string): VerificationCodeView[] {
  if (!Array.isArray(value)) return []
  return value.flatMap((item, index) => {
    const code = codeFrom(item)
    if (!code) return []
    const record = isRecord(item) ? item : {}
    const purpose = asText(pick(record, ['purpose', 'type', 'kind', 'stage'])).toLowerCase()
    if (purpose.includes('login') || purpose.includes('pin')) return []
    if (!purpose && (code === loginCode || code === pinCode) && index < 2) return []
    return [{
      id: asText(pick(record, ['id']), `${index}-${code}`),
      code,
      receivedAt: asText(pick(record, ['received_at', 'receivedAt', 'created_at', 'createdAt'])) || undefined,
    }]
  })
}

export function normalizeActivation(payload: unknown): ActivationView {
  const value = unwrap(payload)
  const raw = isRecord(value) ? value : {}
  let loginCode = codeFrom(pick(raw, ['login_code', 'loginCode', 'login_otp', 'loginOtp']))
  let pinCode = codeFrom(pick(raw, ['pin_code', 'pinCode', 'change_pin_code', 'changePinCode', 'pin_otp', 'pinOtp']))
  const codes = pick(raw, [
    'subsequent_codes',
    'subsequentCodes',
    'verification_codes',
    'verificationCodes',
    'otp_history',
    'otpHistory',
    'codes',
  ])
  if (Array.isArray(codes)) {
    for (const item of codes) {
      if (!isRecord(item)) continue
      const phase = asText(pick(item, ['phase', 'purpose', 'type', 'kind', 'stage'])).toLowerCase()
      const code = codeFrom(item)
      if (!loginCode && phase.includes('login')) loginCode = code
      if (!pinCode && phase.includes('pin')) pinCode = code
    }
  }
  const id = asText(pick(raw, ['id', 'activation_id', 'activationId']))
  return {
    id,
    activationId: asText(pick(raw, ['activation_id', 'activationId', 'provider_id', 'providerId'])) || undefined,
    batchId: asText(pick(raw, ['batch_id', 'batchId'])) || undefined,
    phone: asText(pick(raw, ['phone', 'number', 'phone_number', 'phoneNumber']), '—'),
    service: asText(pick(raw, ['service', 'service_code', 'serviceCode'])) || undefined,
    country: asText(pick(raw, ['country', 'country_code', 'countryCode'])) || undefined,
    provider: asText(pick(raw, ['provider', 'operator', 'vendor', 'supplier', 'provider_id', 'providerId'])) || undefined,
    status: asText(pick(raw, ['status', 'state']), 'pending').toLowerCase(),
    balance: pick(raw, ['balance', 'balance_rp', 'balanceRp']) === undefined
      ? undefined
      : asNumber(pick(raw, ['balance', 'balance_rp', 'balanceRp'])),
    loginCode: loginCode || undefined,
    pinCode: pinCode || undefined,
    subsequentCodes: normalizeCodeList(codes, loginCode, pinCode),
    error: asText(pick(raw, ['error', 'message', 'last_error', 'lastError', 'failure_reason', 'failureReason'])) || undefined,
    createdAt: asText(pick(raw, ['created_at', 'createdAt'])) || undefined,
    expiresAt: asText(pick(raw, [
      'expires_at',
      'expiresAt',
      'expired_at',
      'expiredAt',
      'provider_expires_at',
      'providerExpiresAt',
    ])) || undefined,
    finishedAt: asText(pick(raw, ['finished_at', 'finishedAt'])) || undefined,
    raw,
  }
}

export function normalizeActivations(payload: unknown): ActivationView[] {
  return entries(payload, ['activations', 'items', 'records'])
    .map(([, value]) => normalizeActivation(value))
    .filter((item) => item.id)
}

const statusMap: Record<string, StatusMeta> = {
  pending: { label: '等待处理', type: 'info', active: true },
  purchasing: { label: '购买中', type: 'primary', active: true },
  purchased: { label: '校验号码', type: 'primary', active: true },
  duplicate: { label: '重复号码', type: 'warning', active: false },
  checking_login: { label: '校验号码', type: 'primary', active: true },
  awaiting_login_code: { label: '等待登录验证码', type: 'warning', active: true },
  logging_in: { label: '正在登录', type: 'primary', active: true },
  waiting_login_otp: { label: '等待登录验证码', type: 'warning', active: true },
  pin_required: { label: '需要 PIN，已丢弃', type: 'danger', active: false },
  unregistered: { label: '未注册', type: 'warning', active: false },
  checking_balance: { label: '查询余额', type: 'primary', active: true },
  zero_rp_used: { label: '0RP已被使用', type: 'info', active: false },
  zero_balance_used: { label: '0RP已被使用', type: 'info', active: false },
  preparing_pin: { label: '准备设置 PIN', type: 'primary', active: true },
  waiting_pin_otp: { label: '等待改 PIN 验证码', type: 'warning', active: true },
  awaiting_pin_code: { label: '等待改 PIN 验证码', type: 'warning', active: true },
  setting_pin: { label: '设置 PIN 中', type: 'primary', active: true },
  polling: { label: '持有中', type: 'success', active: true },
  active: { label: '持有中', type: 'success', active: true },
  success: { label: '成功', type: 'success', active: false },
  expired: { label: '过期', type: 'info', active: false },
  cancelled: { label: '已取消', type: 'info', active: false },
  duplicate_cancel_pending: { label: '重复号取消中', type: 'warning', active: true },
  delete_pending: { label: '删除中', type: 'warning', active: true },
  failed: { label: '失败', type: 'danger', active: false },
}

export function activationStatus(status: string): StatusMeta {
  return statusMap[status] ?? {
    label: status || '未知',
    type: 'info',
    active: false,
  }
}

export function batchStatus(status: string): StatusMeta {
  const aliases: Record<string, StatusMeta> = {
    pending: { label: '等待开始', type: 'info', active: true },
    queued: { label: '排队中', type: 'info', active: true },
    running: { label: '执行中', type: 'primary', active: true },
    processing: { label: '执行中', type: 'primary', active: true },
    completed: { label: '已完成', type: 'success', active: false },
    success: { label: '已完成', type: 'success', active: false },
    failed: { label: '失败', type: 'danger', active: false },
    cancelled: { label: '已结束', type: 'info', active: false },
  }
  return aliases[status] ?? { label: status || '未知', type: 'info', active: false }
}

export function formatNumber(value: number): string {
  return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 4 }).format(value)
}

export function formatTime(value?: string): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date)
}
