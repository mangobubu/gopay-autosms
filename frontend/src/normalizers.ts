import type {
  ActivationView,
  BatchView,
  CatalogOption,
  GoPayLoginState,
  GoPayLoginStatusView,
  PriceOption,
  PriceTier,
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

function asOptionalNumber(value: unknown): number | undefined {
  if (typeof value === 'number') return Number.isFinite(value) ? value : undefined
  if (typeof value !== 'string') return undefined
  const normalized = value.trim().replace(',', '.').replace(/[\s₽$€£]/g, '')
  if (!/^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$/.test(normalized)) return undefined
  const parsed = Number.parseFloat(normalized)
  return Number.isFinite(parsed) ? parsed : undefined
}

function numericPriceTier(value: unknown): PriceTier | undefined {
  const text = asText(value).trim()
  if (typeof value !== 'number' && !/^[1-3]$/.test(text)) return undefined
  const numeric = typeof value === 'number' ? value : Number.parseInt(text, 10)
  if (numeric === 1) return 'Gold'
  if (numeric === 2) return 'Silver'
  if (numeric === 3) return 'Bronze'
  return undefined
}

function asPriceTier(value: unknown): PriceTier | undefined {
  const normalized = asText(value).trim().toLowerCase().replace(/[^a-z0-9]/g, '')
  if (['bronze', 'bronzetier', 'bronzerank'].includes(normalized)) return 'Bronze'
  if (['silver', 'silvertier', 'silverrank'].includes(normalized)) return 'Silver'
  if (['gold', 'goldtier', 'goldrank'].includes(normalized)) return 'Gold'
  if (isRecord(value)) {
    for (const key of ['description', 'name', 'tier', 'rank', 'level', 'grade', 'quality']) {
      const nested = asPriceTier(value[key])
      if (nested) return nested
    }
    return numericPriceTier(pick(value, ['id', 'value', 'code']))
  }
  return numericPriceTier(value)
}

const PRICE_TIER_KEYS = [
  'tier',
  'rank',
  'level',
  'grade',
  'quality',
  'providerTier',
  'provider_tier',
  'providerRank',
  'provider_rank',
  'providerLevel',
  'provider_level',
  'providerGrade',
  'provider_grade',
  'providerQuality',
  'provider_quality',
] as const

function priceTierFromRecord(record: UnknownRecord): PriceTier | undefined {
  for (const key of PRICE_TIER_KEYS) {
    const tier = asPriceTier(record[key])
    if (tier) return tier
  }
  return undefined
}

function hasPriceTierMetadata(record: UnknownRecord): boolean {
  return PRICE_TIER_KEYS.some((key) => {
    const value = record[key]
    if (value === undefined || value === null) return false
    return typeof value !== 'string' || value.trim() !== ''
  })
}

function priceOptionLabel(item: PriceOption): string {
  const parts = [item.price === undefined ? '价格待定' : `${formatNumber(item.price)} ₽`]
  if (item.tier) parts.push(item.tier)
  if (item.provider) parts.push(item.provider)
  if (item.stock !== undefined) parts.push(`库存 ${formatNumber(item.stock)}`)
  return parts.join(' · ')
}

function isAvailablePrice(item: PriceOption): boolean {
  return item.price !== undefined && (item.stock === undefined || item.stock > 0)
}

function derivedPriceTier(index: number, count: number): PriceTier {
  if (count < 2 || index === 0) return 'Bronze'
  if (count === 2 || index === count - 1) return 'Gold'

  const tiers: PriceTier[] = ['Bronze', 'Silver', 'Gold']
  return tiers[Math.min(2, Math.floor((index * tiers.length) / count))]
}

function deriveMissingPriceTiers(items: PriceOption[]): PriceOption[] {
  const distinctPrices = Array.from(new Set(
    items
      .filter(isAvailablePrice)
      .map((item) => item.price as number),
  )).sort((left, right) => left - right)
  const tierByPrice = new Map(
    distinctPrices.map((price, index) => [price, derivedPriceTier(index, distinctPrices.length)]),
  )

  return items.map((item) => {
    const canDerive = !item.tier
      && !hasPriceTierMetadata(item.raw)
      && isAvailablePrice(item)
    const tier = canDerive ? tierByPrice.get(item.price as number) : item.tier
    const normalized = {
      ...item,
      tier,
      tierDerived: tier ? canDerive : undefined,
    } satisfies PriceOption
    return {
      ...normalized,
      label: priceOptionLabel(normalized),
    }
  })
}

function asBoolean(value: unknown): boolean {
  if (typeof value === 'boolean') return value
  if (typeof value === 'number') return value !== 0
  if (typeof value === 'string') return ['1', 'true', 'yes'].includes(value.toLowerCase())
  return false
}

function asOptionalBoolean(value: unknown): boolean | undefined {
  if (typeof value === 'boolean') return value
  if (typeof value === 'number') {
    if (value === 1) return true
    if (value === 0) return false
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase()
    if (['1', 'true', 'yes'].includes(normalized)) return true
    if (['0', 'false', 'no'].includes(normalized)) return false
  }
  return undefined
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
    ['id', 'country', 'value', 'code'],
    ['name', 'label', 'title', 'country_name', 'eng', 'english', 'nameEn', 'rus', 'chn'],
  )
}

export function normalizePrices(payload: unknown): PriceOption[] {
  const normalized = entries(payload, ['prices', 'items', 'providers', 'offers'])
    .map(([entryKey, value], index) => {
      const raw = optionRecord(entryKey, value)
      const provider = asText(
        pick(raw, ['provider', 'provider_id', 'providerId', 'operator', 'vendor', 'supplier', 'name']),
      )
      const rawPrice = pick(raw, ['price', 'cost', 'amount', 'rate', 'value'])
      const price = asOptionalNumber(rawPrice)
      const rawStock = pick(raw, ['stock', 'count', 'available', 'quantity'])
      const stock = asOptionalNumber(rawStock)
      const tier = priceTierFromRecord(raw)
      const id = asText(
        pick(raw, ['id', 'code', 'key', 'provider_id', 'providerId']),
        `${provider}:${price ?? entryKey}`,
      )
      return {
        key: `${id}-${index}`,
        value: id,
        label: '',
        description: asText(pick(raw, ['description', 'label', 'title'])),
        price,
        provider,
        stock,
        tier,
        tierDerived: tier ? false : undefined,
        raw,
      } satisfies PriceOption
    })

  const sorted = normalized
    .map((item, originalIndex) => ({ item, originalIndex }))
    .sort((left, right) => {
      const leftPrice = left.item.price
      const rightPrice = right.item.price
      if (leftPrice === undefined && rightPrice === undefined) {
        return left.originalIndex - right.originalIndex
      }
      if (leftPrice === undefined) return 1
      if (rightPrice === undefined) return -1
      return leftPrice - rightPrice || left.originalIndex - right.originalIndex
    })
    .map(({ item }) => item)

  return deriveMissingPriceTiers(sorted)
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
  const purchased = asNumber(pick(record, [
    'purchased',
    'purchased_count',
    'purchasedCount',
    'purchase_count',
    'purchaseCount',
  ]), Math.min(total, successful + inflight + failed))
  const completed = asNumber(
    pick(record, ['completed', 'processed', 'completed_count']),
    successful + failed,
  )
  return {
    id: asText(pick(record, ['id', 'batch_id', 'batchId'])),
    status: asText(pick(record, ['status', 'state']), 'pending').toLowerCase(),
    total,
    purchased,
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

function normalizeLoginState(value: unknown, valid?: boolean): GoPayLoginState {
  const normalized = asText(value).trim().toLowerCase().replace(/[\s-]+/g, '_')
  const aliases: Record<string, GoPayLoginState> = {
    valid: 'valid',
    active: 'valid',
    authenticated: 'valid',
    logged_in: 'valid',
    online: 'valid',
    invalid: 'invalid',
    expired: 'invalid',
    unauthenticated: 'invalid',
    logged_out: 'invalid',
    session_expired: 'invalid',
    revoked: 'invalid',
    checking: 'checking',
    pending: 'checking',
    refreshing: 'checking',
    loading: 'checking',
    unknown: 'unknown',
  }
  if (aliases[normalized]) return aliases[normalized]
  if (valid === true) return 'valid'
  if (valid === false) return 'invalid'
  return 'unknown'
}

export function normalizeAccountLoginStatuses(payload: unknown): GoPayLoginStatusView[] {
  return entries(payload, ['accounts', 'items', 'records'])
    .map<GoPayLoginStatusView | null>(([, value], index) => {
      if (!isRecord(value)) return null
      const valid = asOptionalBoolean(pick(value, ['valid', 'is_valid', 'isValid']))
      const status = normalizeLoginState(
        pick(value, ['login_status', 'loginStatus', 'status', 'state']),
        valid,
      )
      const phone = asText(
        pick(value, ['phone_number', 'phoneNumber', 'phone', 'number']),
      ).trim()
      const id = asText(
        pick(value, ['account_id', 'accountId', 'id']),
        phone || `account-${index + 1}`,
      )
      return {
        id,
        phone: phone || '未提供号码',
        status,
        valid,
        checkedAt: asText(pick(value, ['checked_at', 'checkedAt'])) || undefined,
        message: asText(pick(value, ['message', 'detail', 'error'])) || undefined,
        refreshed: asOptionalBoolean(pick(value, ['refreshed', 'was_refreshed', 'wasRefreshed'])) ?? false,
        raw: value,
      }
    })
    .filter((item): item is GoPayLoginStatusView => item !== null)
}

export function accountLoginStatus(status: GoPayLoginState): StatusMeta {
  const statuses: Record<GoPayLoginState, StatusMeta> = {
    valid: { label: '登录有效', type: 'success', active: false },
    invalid: { label: '登录已失效', type: 'danger', active: false },
    checking: { label: '检查中', type: 'primary', active: true },
    unknown: { label: '状态未知', type: 'info', active: false },
  }
  return statuses[status]
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
  login_failed: { label: '登录失败', type: 'danger', active: false },
  checking_balance: { label: '查询余额', type: 'primary', active: true },
  zero_rp_used: { label: '0RP已被使用', type: 'info', active: false },
  zero_balance_used: { label: '0RP已被使用', type: 'info', active: false },
  preparing_pin: { label: '准备设置 PIN', type: 'primary', active: true },
  waiting_pin_otp: { label: '等待改 PIN 验证码', type: 'warning', active: true },
  awaiting_pin_code: { label: '等待改 PIN 验证码', type: 'warning', active: true },
  setting_pin: { label: '正在设置 PIN', type: 'primary', active: true },
  pin_changed: { label: '改 PIN 成功', type: 'success', active: true },
  awaiting_subsequent_code: { label: '等待后续验证码', type: 'success', active: true },
  polling: { label: '等待后续验证码', type: 'success', active: true },
  active: { label: '等待后续验证码', type: 'success', active: true },
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
    cancelled: { label: '已停止', type: 'info', active: false },
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

export function formatCountdown(value?: string, now = Date.now()): string {
  if (!value || !Number.isFinite(now)) return '—'
  const expiresAt = new Date(value).getTime()
  if (!Number.isFinite(expiresAt)) return '—'

  const totalSeconds = Math.max(0, Math.ceil((expiresAt - now) / 1_000))
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return `${String(minutes).padStart(2, '0')}分${String(seconds).padStart(2, '0')}秒`
}
