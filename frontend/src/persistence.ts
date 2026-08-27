import type { PriceTier } from './types'

export const CLIENT_PERSISTENCE_VERSION = 1 as const
export const API_KEY_STORAGE_KEY = 'gopay-autosms.client.api-key.v1'
export const BATCH_FORM_STORAGE_KEY = 'gopay-autosms.client.batch-form.v1'

const API_KEY_SCHEMA = 'gopay-autosms/api-key'
const BATCH_FORM_SCHEMA = 'gopay-autosms/batch-form'

export interface StorageLike {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
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
  service: string
  country: string
  priceKey: string
  quantity: number
  pin: string
  proxy: string
  priceSnapshot?: ClientPriceSnapshot
}

interface PersistenceEnvelope<T> {
  schema: string
  version: typeof CLIENT_PERSISTENCE_VERSION
  data: T
}

export const DEFAULT_API_KEY = ''
export const DEFAULT_BATCH_FORM: Readonly<ClientBatchForm> = Object.freeze({
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

function browserStorage(): StorageLike | undefined {
  try {
    const storage = globalThis.localStorage
    if (!storage
      || typeof storage.getItem !== 'function'
      || typeof storage.setItem !== 'function'
      || typeof storage.removeItem !== 'function') return undefined
    return storage
  } catch {
    return undefined
  }
}

function resolveStorage(storage?: StorageLike): StorageLike | undefined {
  return storage ?? browserStorage()
}

/** Read a storage entry without leaking browser storage exceptions to the UI. */
export function safeGetItem(storage: StorageLike | undefined, key: string): string | null {
  if (!storage) return null
  try {
    return storage.getItem(key)
  } catch {
    return null
  }
}

/** Write a storage entry and report quota/privacy-mode failures to the caller. */
export function safeSetItem(
  storage: StorageLike | undefined,
  key: string,
  value: string,
): boolean {
  if (!storage) return false
  try {
    storage.setItem(key, value)
    return true
  } catch {
    return false
  }
}

/** Remove a storage entry and report privacy-mode failures to the caller. */
export function safeRemoveItem(storage: StorageLike | undefined, key: string): boolean {
  if (!storage) return false
  try {
    storage.removeItem(key)
    return true
  } catch {
    return false
  }
}

function readEnvelope<T>(
  storage: StorageLike | undefined,
  key: string,
  schema: string,
  normalize: (data: unknown) => T | undefined,
): T | undefined {
  const raw = safeGetItem(storage, key)
  if (raw === null) return undefined

  try {
    const envelope: unknown = JSON.parse(raw)
    if (!isRecord(envelope)
      || envelope.schema !== schema
      || envelope.version !== CLIENT_PERSISTENCE_VERSION) return undefined
    return normalize(envelope.data)
  } catch {
    return undefined
  }
}

function writeEnvelope<T>(
  storage: StorageLike | undefined,
  key: string,
  schema: string,
  data: T,
): boolean {
  const envelope: PersistenceEnvelope<T> = {
    schema,
    version: CLIENT_PERSISTENCE_VERSION,
    data,
  }

  try {
    return safeSetItem(storage, key, JSON.stringify(envelope))
  } catch {
    return false
  }
}

function normalizeApiKey(value: unknown): string | undefined {
  return typeof value === 'string' ? value.trim() : undefined
}

/** Keep the client plaintext when the settings endpoint returns an empty or masked key. */
export function mergeClientApiKey(clientApiKey: string, serverApiKey: string): string {
  const normalizedClient = normalizeApiKey(clientApiKey) ?? DEFAULT_API_KEY
  const normalizedServer = normalizeApiKey(serverApiKey) ?? DEFAULT_API_KEY
  return normalizedServer && !normalizedServer.includes('*')
    ? normalizedServer
    : normalizedClient
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

/**
 * Convert persisted or caller-provided data into the current form schema.
 * Invalid individual fields fall back independently so one bad field does not
 * discard the rest of a user's draft.
 */
export function normalizeBatchForm(value: unknown): ClientBatchForm {
  const defaults = { ...DEFAULT_BATCH_FORM }
  if (!isRecord(value)) return defaults

  const normalized: ClientBatchForm = {
    service: typeof value.service === 'string' ? value.service : defaults.service,
    country: typeof value.country === 'string' ? value.country : defaults.country,
    priceKey: typeof value.priceKey === 'string' ? value.priceKey : defaults.priceKey,
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
  const priceSnapshot = normalizePriceSnapshot(value.priceSnapshot)
  if (priceSnapshot) normalized.priceSnapshot = priceSnapshot
  return normalized
}

export interface ClientPersistence {
  loadApiKey(): string
  saveApiKey(apiKey: string): boolean
  clearApiKey(): boolean
  loadBatchForm(): ClientBatchForm
  saveBatchForm(form: ClientBatchForm): boolean
  clearBatchForm(): boolean
}

/** Create an isolated persistence facade; inject an in-memory storage in tests. */
export function createClientPersistence(storage?: StorageLike): ClientPersistence {
  const target = resolveStorage(storage)
  return {
    loadApiKey: () => readEnvelope(
      target,
      API_KEY_STORAGE_KEY,
      API_KEY_SCHEMA,
      normalizeApiKey,
    ) ?? DEFAULT_API_KEY,
    saveApiKey: (apiKey) => writeEnvelope(
      target,
      API_KEY_STORAGE_KEY,
      API_KEY_SCHEMA,
      normalizeApiKey(apiKey) ?? DEFAULT_API_KEY,
    ),
    clearApiKey: () => safeRemoveItem(target, API_KEY_STORAGE_KEY),
    loadBatchForm: () => readEnvelope(
      target,
      BATCH_FORM_STORAGE_KEY,
      BATCH_FORM_SCHEMA,
      normalizeBatchForm,
    ) ?? { ...DEFAULT_BATCH_FORM },
    saveBatchForm: (form) => writeEnvelope(
      target,
      BATCH_FORM_STORAGE_KEY,
      BATCH_FORM_SCHEMA,
      normalizeBatchForm(form),
    ),
    clearBatchForm: () => safeRemoveItem(target, BATCH_FORM_STORAGE_KEY),
  }
}

export function loadApiKey(storage?: StorageLike): string {
  return createClientPersistence(storage).loadApiKey()
}

export function saveApiKey(apiKey: string, storage?: StorageLike): boolean {
  return createClientPersistence(storage).saveApiKey(apiKey)
}

export function clearApiKey(storage?: StorageLike): boolean {
  return createClientPersistence(storage).clearApiKey()
}

export function loadBatchForm(storage?: StorageLike): ClientBatchForm {
  return createClientPersistence(storage).loadBatchForm()
}

export function saveBatchForm(form: ClientBatchForm, storage?: StorageLike): boolean {
  return createClientPersistence(storage).saveBatchForm(form)
}

export function clearBatchForm(storage?: StorageLike): boolean {
  return createClientPersistence(storage).clearBatchForm()
}

export const getPersistedApiKey = loadApiKey
export const setPersistedApiKey = saveApiKey
export const getPersistedBatchForm = loadBatchForm
export const setPersistedBatchForm = saveBatchForm
