import { isSMSProvider } from './smsProviders.ts'
import type { PriceTier, SMSProvider } from './types'

// These keys are kept only so clients upgrading from earlier releases can
// remove plaintext data that was previously stored in the browser.
export const API_KEY_STORAGE_KEY = 'gopay-autosms.client.api-key.v1'
export const SMSBOWER_API_KEY_STORAGE_KEY = API_KEY_STORAGE_KEY
export const HERO_SMS_API_KEY_STORAGE_KEY = 'gopay-autosms.client.hero-sms-api-key.v1'
export const BATCH_FORM_STORAGE_KEY = 'gopay-autosms.client.batch-form.v1'
export const ACTIVE_BATCH_STORAGE_KEY = 'gopay-autosms.active-batch'

export const LEGACY_CLIENT_STORAGE_KEYS = Object.freeze([
  API_KEY_STORAGE_KEY,
  HERO_SMS_API_KEY_STORAGE_KEY,
  BATCH_FORM_STORAGE_KEY,
  ACTIVE_BATCH_STORAGE_KEY,
])

export interface StorageLike {
  removeItem(key: string): void
}

export interface ClientPriceSnapshot {
  value: string
  provider?: string
  price?: number
  tier?: PriceTier
  tierDerived?: boolean
}

export interface ClientBatchForm {
  smsProvider: SMSProvider
  service: string
  country: string
  priceKey: string
  quantity: number
  pin: string
  proxy: string
  priceSnapshot?: ClientPriceSnapshot
}

export const DEFAULT_BATCH_FORM: Readonly<ClientBatchForm> = Object.freeze({
  smsProvider: 'smsbower',
  service: '',
  country: '',
  priceKey: '',
  quantity: 1,
  pin: '',
  proxy: '',
})

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function normalizePriceSnapshot(value: unknown): ClientPriceSnapshot | undefined {
  if (!isRecord(value) || typeof value.value !== 'string' || !value.value.trim()) return undefined

  const snapshot: ClientPriceSnapshot = { value: value.value }
  if (typeof value.provider === 'string') snapshot.provider = value.provider
  if (typeof value.price === 'number' && Number.isFinite(value.price) && value.price >= 0) {
    snapshot.price = value.price
  }
  if (value.tier === 'Bronze' || value.tier === 'Silver' || value.tier === 'Gold') {
    snapshot.tier = value.tier
  }
  if (typeof value.tierDerived === 'boolean') snapshot.tierDerived = value.tierDerived
  return snapshot
}

/** Normalize a server-provided draft into the current client form schema. */
export function normalizeBatchForm(value: unknown): ClientBatchForm {
  const defaults = { ...DEFAULT_BATCH_FORM }
  if (!isRecord(value)) return defaults

  const rawSMSProvider = value.smsProvider ?? value.sms_provider
  const smsProvider = isSMSProvider(rawSMSProvider) ? rawSMSProvider : defaults.smsProvider
  const normalized: ClientBatchForm = {
    smsProvider,
    service: typeof value.service === 'string' ? value.service : defaults.service,
    country: typeof value.country === 'string' ? value.country : defaults.country,
    priceKey: typeof value.priceKey === 'string'
      ? value.priceKey
      : typeof value.price_key === 'string' ? value.price_key : defaults.priceKey,
    quantity: typeof value.quantity === 'number'
      && Number.isInteger(value.quantity)
      && value.quantity >= 1
      && value.quantity <= 100
      ? value.quantity
      : defaults.quantity,
    pin: typeof value.pin === 'string' && /^\d{0,6}$/.test(value.pin)
      ? value.pin
      : defaults.pin,
    proxy: typeof value.proxy === 'string' ? value.proxy : defaults.proxy,
  }
  const priceSnapshot = normalizePriceSnapshot(value.priceSnapshot ?? value.price_snapshot)
  if (priceSnapshot) {
    if (smsProvider === 'hero-sms') {
      delete priceSnapshot.provider
      delete priceSnapshot.tier
      delete priceSnapshot.tierDerived
    }
    normalized.priceSnapshot = priceSnapshot
  }
  return normalized
}

function browserStorage(): StorageLike | undefined {
  try {
    const storage = globalThis.localStorage
    return storage && typeof storage.removeItem === 'function' ? storage : undefined
  } catch {
    return undefined
  }
}

/** Remove plaintext browser state left behind by releases that used localStorage. */
export function clearLegacyClientPersistence(storage?: StorageLike): boolean {
  const target = storage ?? browserStorage()
  if (!target) return false

  let cleared = true
  for (const key of LEGACY_CLIENT_STORAGE_KEYS) {
    try {
      target.removeItem(key)
    } catch {
      cleared = false
    }
  }
  return cleared
}
