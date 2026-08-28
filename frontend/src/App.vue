<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'

import { api, ApiError } from './api'
import { matchesCatalogOption } from './catalogSearch'
import ActivationCard from './components/ActivationCard.vue'
import ActivationTable from './components/ActivationTable.vue'
import {
  findAccountLoginStatusByPhone,
  indexAccountLoginStatusesByPhone,
} from './loginStatus'
import { findRefreshedPrice } from './priceOptions'
import { SMS_PROVIDERS, smsProviderProfile } from './smsProviders'
import {
  batchStatus,
  isSuccessfulActivation,
  normalizeAccountLoginStatuses,
  normalizeActivations,
  normalizeBatch,
  normalizeCountries,
  normalizePrices,
  normalizeServices,
  normalizeSettings,
} from './normalizers'
import {
  clearLegacyClientPersistence,
  DEFAULT_BATCH_FORM,
  normalizeBatchForm,
} from './persistence'
import type { ClientBatchForm, ClientPriceSnapshot } from './persistence'
import type {
  ActivationView,
  BatchView,
  CatalogOption,
  GoPayLoginStatusView,
  PriceOption,
  PriceTier,
  SMSProvider,
  SMSProviderSettings,
} from './types'

const POLL_INTERVAL_MS = 2_000
// The server's default login-status cache is four seconds, so each normal
// five-second browser poll can start a fresh remote probe without a boundary hit.
const LOGIN_STATUS_POLL_INTERVAL_MS = 5_000
const LOGIN_STATUS_REQUEST_TIMEOUT_MS = 20_000
const CLOCK_INTERVAL_MS = 1_000
const BATCH_DRAFT_SAVE_DELAY_MS = 500
const BATCH_DRAFT_RETRY_MAX_MS = 30_000
type ServiceCatalogLoadResult = 'loaded' | 'failed' | 'stale'
type PendingBatchDraft = { draft: ClientBatchForm, signature: string, keepalive: boolean }

let legacyClientPersistenceCleared = clearLegacyClientPersistence()

const smsProvider = ref<SMSProvider>(DEFAULT_BATCH_FORM.smsProvider)
const settingsByProvider = reactive<Record<SMSProvider, SMSProviderSettings>>({
  smsbower: {
    apiKey: '',
    configured: false,
  },
  'hero-sms': {
    apiKey: '',
    configured: false,
  },
})
const settings = computed(() => settingsByProvider[smsProvider.value])
const currentSMSProviderProfile = computed(() => smsProviderProfile(smsProvider.value))
const supportsPriceTiers = computed(() => currentSMSProviderProfile.value.supportsPriceTiers)
const priceFieldLabel = computed(() => (
  smsProvider.value === 'hero-sms' ? '价格' : '价格 / 供应商'
))
const settingsLoading = ref(false)
const settingsSaving = ref(false)
const settingsReady = computed(() => Boolean(settings.value.configured))

const services = ref<CatalogOption[]>([])
const countries = ref<CatalogOption[]>([])
const prices = ref<PriceOption[]>([])
const servicesLoading = ref(false)
const countriesLoading = ref(false)
const pricesLoading = ref(false)
const catalogReady = ref(false)
const countryQuery = ref('')

const batchFormRef = ref<FormInstance>()
const form = reactive({
  service: DEFAULT_BATCH_FORM.service,
  country: DEFAULT_BATCH_FORM.country,
  priceKey: DEFAULT_BATCH_FORM.priceKey,
  quantity: DEFAULT_BATCH_FORM.quantity,
  pin: DEFAULT_BATCH_FORM.pin,
  proxy: DEFAULT_BATCH_FORM.proxy,
})

const proxyInputCount = computed(() => form.proxy.split(/\r?\n/).map((item) => item.trim()).filter(Boolean).length)
const proxyPlaceholder = [
  'hostname:port:username:password',
  'socks5://username:password@host:port',
  'username:password@hostname:port',
  'hostname:port@username:password',
].join('\n')

const rules: FormRules = {
  service: [{ required: true, message: '请选择服务', trigger: 'change' }],
  country: [{ required: true, message: '请选择国家', trigger: 'change' }],
  priceKey: [{ required: true, message: '请选择价格', trigger: 'change' }],
  quantity: [
    { required: true, message: '请输入计划购买数量', trigger: 'change' },
    {
      validator: (_rule, value, callback) => {
        if (!Number.isInteger(value) || value < 1 || value > 100) {
          callback(new Error('购买数量范围为 1–100'))
          return
        }
        callback()
      },
      trigger: 'change',
    },
  ],
  pin: [
    { required: true, message: '请输入 6 位 PIN', trigger: 'blur' },
    { pattern: /^\d{6}$/, message: 'PIN 必须是 6 位数字', trigger: ['blur', 'change'] },
  ],
}

const starting = ref(false)
const stoppingBatch = ref(false)
const refreshing = ref(false)
const connectionError = ref('')
const batchDraftLoadError = ref('')
const batchDraftSaveError = ref('')
const visibleConnectionError = computed(() => (
  batchDraftLoadError.value || batchDraftSaveError.value || connectionError.value
))
const currentBatch = ref<BatchView | null>(null)
const currentBatchId = ref('')
const activeBatchLoading = ref(true)
const activations = ref<ActivationView[]>([])
const actionBusy = reactive<Record<string, boolean>>({})
const countdownNow = ref(Date.now())
const accountLoginStatuses = ref<GoPayLoginStatusView[]>([])
const loginStatusRefreshing = ref(false)
let pollTimer: number | undefined
let loginStatusPollTimer: number | undefined
let clockTimer: number | undefined
let batchDraftSaveTimer: number | undefined
let batchDraftSaveRetryTimer: number | undefined
let batchDraftLoadRetryTimer: number | undefined
let loginStatusAbortController: AbortController | undefined
let disposed = false
let batchDraftPersistenceReady = false
let lastPersistedBatchDraftSignature = ''
let lastQueuedBatchDraftSignature = ''
let batchDraftSaveQueue: Promise<void> = Promise.resolve()
let batchDraftSaveInFlight = false
let batchDraftSaveRetryAttempt = 0
let batchDraftLoadRetryAttempt = 0
let pendingBatchDraft: PendingBatchDraft | undefined
let catalogRestoreVersion = 0
let catalogActionVersion = 0
let serviceRequestVersion = 0
let countryRequestVersion = 0
let priceRequestVersion = 0
let settingsRequestVersion = 0
let dashboardGeneration = 0
let lastPriceSnapshot: ClientPriceSnapshot | undefined
let lastPriceSnapshotKey = ''
let heroSmsNavigationPending = false

const selectedPrice = computed(() => prices.value.find((item) => item.key === form.priceKey))
const selectedService = computed(() => services.value.find((item) => item.value === form.service))
const selectedCountry = computed(() => countries.value.find((item) => item.value === form.country))
const filteredCountries = computed(() => countries.value.filter(
  (item) => matchesCatalogOption(item, countryQuery.value),
))
const accountLoginStatusesByPhone = computed(() => (
  indexAccountLoginStatusesByPhone(accountLoginStatuses.value)
))
const currentBatchMeta = computed(() => batchStatus(currentBatch.value?.status ?? 'pending'))
const currentBatchActive = computed(() => Boolean(currentBatch.value && currentBatchMeta.value.active))
const canStopCurrentBatch = computed(() => (
  currentBatch.value?.status === 'pending' || currentBatch.value?.status === 'running'
))
const successfulActivations = computed(() => activations.value.filter(isSuccessfulActivation))
const remainingActivations = computed(() => (
  activations.value.filter((item) => !isSuccessfulActivation(item))
))
const unsuccessfulCount = computed(() => {
  const batch = currentBatch.value
  if (!batch) return 0
  return Math.max(0, batch.purchased - batch.successful - batch.inflight)
})
const batchProgress = computed(() => {
  const batch = currentBatch.value
  if (!batch || batch.total < 1) return 0
  return Math.min(100, Math.round((batch.successful / batch.total) * 100))
})

function snapshotPrice(item: PriceOption): ClientPriceSnapshot {
  const snapshot: ClientPriceSnapshot = { value: item.value }
  if (item.provider !== undefined) snapshot.provider = item.provider
  if (item.price !== undefined) snapshot.price = item.price
  if (item.tier !== undefined) snapshot.tier = item.tier
  if (item.tierDerived !== undefined) snapshot.tierDerived = item.tierDerived
  return snapshot
}

function currentBatchFormDraft(): ClientBatchForm {
  const price = selectedPrice.value
  if (price) {
    lastPriceSnapshot = snapshotPrice(price)
    lastPriceSnapshotKey = form.priceKey
  } else if (!form.priceKey || form.priceKey !== lastPriceSnapshotKey) {
    lastPriceSnapshot = undefined
    lastPriceSnapshotKey = form.priceKey
  }

  const draft: ClientBatchForm = {
    smsProvider: smsProvider.value,
    service: form.service,
    country: form.country,
    priceKey: form.priceKey,
    quantity: form.quantity,
    pin: form.pin,
    proxy: form.proxy,
  }
  if (lastPriceSnapshot) draft.priceSnapshot = lastPriceSnapshot
  return draft
}

function batchDraftSignature(draft: ClientBatchForm): string {
  return JSON.stringify(normalizeBatchForm(draft))
}

function applyBatchFormDraft(draft: ClientBatchForm): void {
  lastPriceSnapshot = draft.priceSnapshot ? { ...draft.priceSnapshot } : undefined
  lastPriceSnapshotKey = draft.priceKey
  smsProvider.value = draft.smsProvider
  Object.assign(form, {
    service: draft.service,
    country: draft.country,
    priceKey: draft.priceKey,
    quantity: draft.quantity,
    pin: draft.pin,
    proxy: draft.proxy,
  })
}

function retryDelay(attempt: number): number {
  return Math.min(BATCH_DRAFT_RETRY_MAX_MS, 1_000 * (2 ** Math.min(attempt, 5)))
}

function scheduleBatchDraftSaveRetry(): void {
  if (disposed || !batchDraftPersistenceReady || !pendingBatchDraft
    || batchDraftSaveRetryTimer !== undefined) return
  const delay = retryDelay(batchDraftSaveRetryAttempt)
  batchDraftSaveRetryTimer = window.setTimeout(() => {
    batchDraftSaveRetryTimer = undefined
    void flushBatchDraftSave()
  }, delay)
}

function flushBatchDraftSave(): Promise<void> {
  if (!batchDraftPersistenceReady || !pendingBatchDraft) return Promise.resolve()
  if (batchDraftSaveInFlight) return batchDraftSaveQueue
  if (batchDraftSaveRetryTimer !== undefined) {
    window.clearTimeout(batchDraftSaveRetryTimer)
    batchDraftSaveRetryTimer = undefined
  }

  batchDraftSaveInFlight = true
  batchDraftSaveQueue = (async () => {
    while (pendingBatchDraft) {
      const pending: PendingBatchDraft = pendingBatchDraft
      try {
        const saved = normalizeBatchForm(await api.saveBatchDraft(pending.draft, pending.keepalive))
        lastPersistedBatchDraftSignature = batchDraftSignature(saved)
        if (pendingBatchDraft?.signature === pending.signature) pendingBatchDraft = undefined
        lastQueuedBatchDraftSignature = pendingBatchDraft?.signature ?? lastPersistedBatchDraftSignature
        batchDraftSaveRetryAttempt = 0
        batchDraftSaveError.value = ''
      } catch (error) {
        batchDraftSaveRetryAttempt += 1
        batchDraftSaveError.value = `任务草稿保存失败，正在后台重试：${friendlyError(error)}`
        scheduleBatchDraftSaveRetry()
        return
      }
    }
  })().finally(() => {
    batchDraftSaveInFlight = false
  })
  return batchDraftSaveQueue
}

function enqueueBatchDraftSave(draft: ClientBatchForm, keepalive = false): Promise<void> {
  const normalized = normalizeBatchForm(draft)
  const signature = batchDraftSignature(normalized)
  if (signature === lastPersistedBatchDraftSignature && !pendingBatchDraft) return Promise.resolve()
  if (signature === lastQueuedBatchDraftSignature && pendingBatchDraft) {
    pendingBatchDraft.keepalive ||= keepalive
    return flushBatchDraftSave()
  }
  lastQueuedBatchDraftSignature = signature
  pendingBatchDraft = { draft: normalized, signature, keepalive }
  return flushBatchDraftSave()
}

function scheduleBatchDraftSave(): void {
  if (!batchDraftPersistenceReady || disposed) return
  if (batchDraftSaveTimer !== undefined) window.clearTimeout(batchDraftSaveTimer)
  batchDraftSaveTimer = window.setTimeout(() => {
    batchDraftSaveTimer = undefined
    void enqueueBatchDraftSave(currentBatchFormDraft())
  }, BATCH_DRAFT_SAVE_DELAY_MS)
}

async function openHeroSmsPage(): Promise<void> {
  if (heroSmsNavigationPending) return
  heroSmsNavigationPending = true
  if (batchDraftSaveTimer !== undefined) {
    window.clearTimeout(batchDraftSaveTimer)
    batchDraftSaveTimer = undefined
  }
  if (batchDraftPersistenceReady) await enqueueBatchDraftSave(currentBatchFormDraft())
  await batchDraftSaveQueue
  if (pendingBatchDraft) {
    heroSmsNavigationPending = false
    ElMessage.error('任务草稿尚未写入数据库，请等待后台重试成功后再打开新页面')
    return
  }
  window.location.assign('/hero-sms')
}

async function loadBatchFormDraft(): Promise<ClientBatchForm | null> {
  try {
    const draft = normalizeBatchForm(await api.getBatchDraft())
    batchDraftLoadError.value = ''
    batchDraftLoadRetryAttempt = 0
    return draft
  } catch (error) {
    batchDraftLoadError.value = `任务草稿读取失败，自动保存已暂停并会继续重试：${friendlyError(error)}`
    return null
  }
}

function activateBatchDraftPersistence(draft: ClientBatchForm): void {
  applyBatchFormDraft(draft)
  lastPersistedBatchDraftSignature = batchDraftSignature(draft)
  lastQueuedBatchDraftSignature = lastPersistedBatchDraftSignature
  batchDraftPersistenceReady = true
  scheduleBatchDraftSave()
}

function scheduleBatchDraftLoadRetry(): void {
  if (disposed || batchDraftPersistenceReady || batchDraftLoadRetryTimer !== undefined) return
  const delay = retryDelay(batchDraftLoadRetryAttempt)
  batchDraftLoadRetryAttempt += 1
  batchDraftLoadRetryTimer = window.setTimeout(async () => {
    batchDraftLoadRetryTimer = undefined
    const draft = await loadBatchFormDraft()
    if (disposed) return
    if (!draft) {
      scheduleBatchDraftLoadRetry()
      return
    }
    const changedWhileOffline = batchDraftSignature(currentBatchFormDraft())
      !== batchDraftSignature(DEFAULT_BATCH_FORM)
    activateBatchDraftPersistence(draft)
    if (settingsReady.value) await restoreBatchFormCatalog(draft)
    if (changedWhileOffline) ElMessage.warning('数据库连接已恢复，页面已重新载入数据库中的任务草稿')
  }, delay)
}

function handleBrowserOnline(): void {
  if (!legacyClientPersistenceCleared) {
    legacyClientPersistenceCleared = clearLegacyClientPersistence()
  }
  if (!batchDraftPersistenceReady) scheduleBatchDraftLoadRetry()
  if (pendingBatchDraft) void flushBatchDraftSave()
}

function flushBatchDraftOnPageHide(): void {
  if (batchDraftPersistenceReady) void enqueueBatchDraftSave(currentBatchFormDraft(), true)
}

watch(() => ({
  smsProvider: smsProvider.value,
  service: form.service,
  country: form.country,
  priceKey: form.priceKey,
  quantity: form.quantity,
  pin: form.pin,
  proxy: form.proxy,
  priceSnapshot: selectedPrice.value ? snapshotPrice(selectedPrice.value) : undefined,
}), scheduleBatchDraftSave, { deep: true })

function draftForCatalogRestore(): ClientBatchForm {
  return currentBatchFormDraft()
}

function cancelBatchFormCatalogRestore(): void {
  catalogRestoreVersion += 1
  catalogActionVersion += 1
}

function finishBatchFormCatalogRestore(
  restoreVersion: number,
  provider: SMSProvider,
  ready = true,
): void {
  if (restoreVersion !== catalogRestoreVersion || provider !== smsProvider.value) return
  catalogReady.value = ready
}

function friendlyError(error: unknown): string {
  if (error instanceof ApiError) return error.message
  return error instanceof Error ? error.message : '发生未知错误'
}

function sanitizePin(value: string): void {
  form.pin = value.replace(/\D/g, '').slice(0, 6)
}

function filterCountries(query: string): void {
  countryQuery.value = query
}

function resetCountryQuery(): void {
  countryQuery.value = ''
}

async function restoreLatestBatchFromServer(): Promise<void> {
  try {
    const payload = await api.getBatches()
    const root = payload && typeof payload === 'object' && !Array.isArray(payload)
      ? payload as Record<string, unknown>
      : undefined
    const rawBatches = Array.isArray(root?.batches)
      ? root.batches
      : Array.isArray(payload) ? payload : []
    const batches = rawBatches.map((item) => normalizeBatch(item)).filter((batch) => batch.id)
    const restoredBatch = batches.find((batch) => (
      batch.status === 'pending' || batch.status === 'running'
    )) ?? batches[0]
    if (!restoredBatch || disposed) return

    if (currentBatchId.value !== restoredBatch.id || currentBatch.value?.status !== restoredBatch.status) {
      dashboardGeneration += 1
      currentBatchId.value = restoredBatch.id
      currentBatch.value = restoredBatch
      activations.value = []
    }
  } catch {
    // The regular dashboard request below still restores a persisted task when available.
  }
}

function handleCountryDropdownVisible(visible: boolean): void {
  if (!visible) resetCountryQuery()
}

function priceOptionLabel(item: PriceOption): string {
  return item.label
}

function priceOptionDetails(item: PriceOption): string {
  if (!item.tier) return item.label
  const tierPart = ` · ${item.tier}`
  return item.label.replace(tierPart, '')
}

function priceTierClass(tier: PriceTier): string {
  return `price-tier--${tier.toLowerCase()}`
}

function priceTierTitle(item: PriceOption): string {
  return item.tierDerived
    ? '价格档位（派生）：数据源未提供供应商等级，按当前可用报价的相对价格计算'
    : '供应商等级（数据源）'
}

function resetPrices(): void {
  priceRequestVersion += 1
  pricesLoading.value = false
  form.priceKey = ''
  prices.value = []
}

async function loadPrices(options: {
  preserveSelection?: boolean
  notifySuccess?: boolean
} = {}): Promise<boolean> {
  const service = form.service
  const country = form.country
  const provider = smsProvider.value
  if (!service || !country) return false

  const previousSelection = options.preserveSelection ? selectedPrice.value : undefined
  const hadSelection = Boolean(form.priceKey)
  const requestVersion = ++priceRequestVersion
  pricesLoading.value = true
  try {
    const nextPrices = normalizePrices(
      await api.getPrices(service, country, provider),
      {
        includeTiers: smsProviderProfile(provider).supportsPriceTiers,
        includeProviders: provider !== 'hero-sms',
        currencyLabel: smsProviderProfile(provider).priceCurrencyLabel,
      },
    )
    if (
      requestVersion !== priceRequestVersion
      || provider !== smsProvider.value
      || service !== form.service
      || country !== form.country
    ) return false

    prices.value = nextPrices
    if (options.preserveSelection && previousSelection) {
      const refreshedSelection = findRefreshedPrice(previousSelection, nextPrices)
      if (refreshedSelection) {
        form.priceKey = refreshedSelection.key
      } else {
        form.priceKey = ''
        ElMessage.warning('原报价已失效，请重新选择')
      }
    } else if (options.preserveSelection && hadSelection) {
      form.priceKey = ''
      ElMessage.warning('原报价已失效，请重新选择')
    }

    connectionError.value = ''
    if (options.notifySuccess) {
      ElMessage.success(`价格已刷新，共 ${nextPrices.length} 个报价`)
    }
    return true
  } catch (error) {
    if (requestVersion === priceRequestVersion && provider === smsProvider.value) {
      ElMessage.error(friendlyError(error))
    }
    return false
  } finally {
    if (requestVersion === priceRequestVersion && provider === smsProvider.value) {
      pricesLoading.value = false
    }
  }
}

async function refreshPrices(): Promise<void> {
  const actionVersion = ++catalogActionVersion
  const service = form.service
  const country = form.country
  const provider = smsProvider.value
  catalogReady.value = false
  const loaded = await loadPrices({ preserveSelection: true, notifySuccess: true })
  if (actionVersion === catalogActionVersion
    && provider === smsProvider.value
    && service === form.service
    && country === form.country) catalogReady.value = loaded
}

async function loadSettings(provider: SMSProvider = smsProvider.value): Promise<void> {
  const requestVersion = ++settingsRequestVersion
  settingsLoading.value = true
  try {
    const payload = await api.getSettings(provider)
    if (requestVersion !== settingsRequestVersion || provider !== smsProvider.value || disposed) return
    const loaded = normalizeSettings(payload)
    const providerSettings = settingsByProvider[provider]
    providerSettings.configured = loaded.configured
    // The API key is stored by the server. The browser only displays the masked
    // value returned by the settings endpoint.
    providerSettings.apiKey = loaded.apiKey
    connectionError.value = ''
  } catch (error) {
    if (requestVersion === settingsRequestVersion && provider === smsProvider.value && !disposed) {
      connectionError.value = friendlyError(error)
    }
  } finally {
    if (requestVersion === settingsRequestVersion && provider === smsProvider.value) {
      settingsLoading.value = false
    }
  }
}

async function saveSettings(): Promise<void> {
  if (settingsSaving.value) return
  const provider = smsProvider.value
  const profile = smsProviderProfile(provider)
  const providerSettings = settingsByProvider[provider]
  if (!providerSettings.apiKey.trim() && !providerSettings.configured) {
    ElMessage.warning(`请输入 ${profile.displayName} API Key`)
    return
  }
  settingsSaving.value = true
  try {
    const apiKey = providerSettings.apiKey.includes('*') ? '' : providerSettings.apiKey.trim()
    await api.saveSettings({
      apiKey,
    }, provider)
    if (provider !== smsProvider.value || disposed) return
    connectionError.value = ''
    ElMessage.success(`${profile.displayName} 配置已保存`)
    await loadSettings(provider)
    if (provider !== smsProvider.value || disposed) return
    if (providerSettings.configured) {
      await restoreBatchFormCatalog(draftForCatalogRestore())
    } else {
      cancelBatchFormCatalogRestore()
      catalogReady.value = false
    }
  } catch (error) {
    if (provider === smsProvider.value && !disposed) ElMessage.error(friendlyError(error))
  } finally {
    settingsSaving.value = false
  }
}

async function loadServices(): Promise<ServiceCatalogLoadResult> {
  const provider = smsProvider.value
  const requestVersion = ++serviceRequestVersion
  servicesLoading.value = true
  try {
    const nextServices = normalizeServices(await api.getServices(provider))
    if (requestVersion !== serviceRequestVersion
      || provider !== smsProvider.value
      || disposed) return 'stale'
    services.value = nextServices
    connectionError.value = ''
    return 'loaded'
  } catch (error) {
    if (requestVersion !== serviceRequestVersion
      || provider !== smsProvider.value
      || disposed) return 'stale'
    services.value = []
    connectionError.value = friendlyError(error)
    return 'failed'
  } finally {
    if (requestVersion === serviceRequestVersion && provider === smsProvider.value) {
      servicesLoading.value = false
    }
  }
}

async function loadCountriesForCurrentService(): Promise<boolean> {
  const provider = smsProvider.value
  const requestVersion = ++countryRequestVersion
  countryQuery.value = ''
  form.country = ''
  countries.value = []
  resetPrices()
  const service = form.service
  if (!service) {
    countriesLoading.value = false
    return true
  }

  countriesLoading.value = true
  try {
    const nextCountries = normalizeCountries(await api.getCountries(service, provider))
    if (requestVersion !== countryRequestVersion
      || provider !== smsProvider.value
      || service !== form.service) return false
    countries.value = nextCountries
    connectionError.value = ''
    return true
  } catch (error) {
    if (requestVersion === countryRequestVersion && provider === smsProvider.value) {
      ElMessage.error(friendlyError(error))
    }
    return false
  } finally {
    if (requestVersion === countryRequestVersion && provider === smsProvider.value) {
      countriesLoading.value = false
    }
  }
}

async function handleServiceChange(): Promise<void> {
  catalogReady.value = false
  cancelBatchFormCatalogRestore()
  const actionVersion = catalogActionVersion
  const provider = smsProvider.value
  const loaded = await loadCountriesForCurrentService()
  if (actionVersion === catalogActionVersion && provider === smsProvider.value) {
    catalogReady.value = loaded
  }
}

async function loadPricesForCurrentCountry(): Promise<boolean> {
  resetPrices()
  if (!form.service || !form.country) return true
  return loadPrices()
}

async function handleCountryChange(): Promise<void> {
  catalogReady.value = false
  cancelBatchFormCatalogRestore()
  const actionVersion = catalogActionVersion
  const provider = smsProvider.value
  const loaded = await loadPricesForCurrentCountry()
  if (actionVersion === catalogActionVersion && provider === smsProvider.value) {
    catalogReady.value = loaded
  }
}

function handlePriceChange(): void {
  cancelBatchFormCatalogRestore()
}

async function handleSMSProviderChange(provider: SMSProvider): Promise<void> {
  settingsRequestVersion += 1
  serviceRequestVersion += 1
  countryRequestVersion += 1
  priceRequestVersion += 1
  cancelBatchFormCatalogRestore()

  settingsLoading.value = false
  servicesLoading.value = false
  countriesLoading.value = false
  pricesLoading.value = false
  catalogReady.value = false
  countryQuery.value = ''
  services.value = []
  countries.value = []
  prices.value = []
  lastPriceSnapshot = undefined
  lastPriceSnapshotKey = ''
  form.service = ''
  form.country = ''
  form.priceKey = ''
  connectionError.value = ''

  const draft = currentBatchFormDraft()
  await loadSettings(provider)
  if (disposed || provider !== smsProvider.value) return
  if (settingsByProvider[provider].configured) {
    await restoreBatchFormCatalog(draft)
  } else {
    catalogReady.value = false
  }
}

function priceFromSnapshot(snapshot: ClientPriceSnapshot, priceKey: string): PriceOption {
  return {
    key: priceKey,
    value: snapshot.value,
    label: '',
    provider: snapshot.provider,
    price: snapshot.price,
    tier: snapshot.tier,
    tierDerived: snapshot.tierDerived,
    raw: {},
  }
}

async function restoreBatchFormCatalog(draft: ClientBatchForm): Promise<void> {
  const provider = smsProvider.value
  if (!settingsByProvider[provider].configured || draft.smsProvider !== provider) return
  const restoreVersion = ++catalogRestoreVersion
  catalogActionVersion += 1
  catalogReady.value = false
  countryRequestVersion += 1
  priceRequestVersion += 1
  countriesLoading.value = false
  pricesLoading.value = false
  countries.value = []
  prices.value = []
  form.service = draft.service
  form.country = draft.country
  form.priceKey = draft.priceKey

  const servicesLoaded = await loadServices()
  if (disposed
    || restoreVersion !== catalogRestoreVersion
    || provider !== smsProvider.value) return
  if (servicesLoaded === 'stale') return
  if (servicesLoaded === 'failed') {
    form.service = draft.service
    form.country = draft.country
    form.priceKey = draft.priceKey
    finishBatchFormCatalogRestore(restoreVersion, provider, false)
    return
  }

  const service = draft.service
  const country = draft.country
  form.service = services.value.some((item) => item.value === service) ? service : ''
  if (!form.service) {
    form.country = ''
    form.priceKey = ''
    finishBatchFormCatalogRestore(restoreVersion, provider)
    return
  }

  const countriesLoaded = await loadCountriesForCurrentService()
  if (disposed
    || restoreVersion !== catalogRestoreVersion
    || provider !== smsProvider.value) return
  if (!countriesLoaded) {
    form.country = country
    form.priceKey = draft.priceKey
    finishBatchFormCatalogRestore(restoreVersion, provider, false)
    return
  }
  form.country = countries.value.some((item) => item.value === country) ? country : ''
  if (!form.country) {
    finishBatchFormCatalogRestore(restoreVersion, provider)
    return
  }

  const pricesLoaded = await loadPricesForCurrentCountry()
  if (disposed
    || restoreVersion !== catalogRestoreVersion
    || provider !== smsProvider.value) return
  if (!pricesLoaded) {
    form.priceKey = draft.priceKey
    finishBatchFormCatalogRestore(restoreVersion, provider, false)
    return
  }
  const exact = prices.value.find((item) => (
    item.key === draft.priceKey
    && item.price !== undefined
    && (item.stock === undefined || item.stock > 0)
  ))
  const priceSnapshot = draft.priceSnapshot
  const refreshed = priceSnapshot
    ? findRefreshedPrice(priceFromSnapshot(priceSnapshot, draft.priceKey), prices.value)
    : undefined
  const restoredPrice = priceSnapshot ? refreshed : exact
  form.priceKey = restoredPrice?.key ?? ''
  lastPriceSnapshot = restoredPrice ? snapshotPrice(restoredPrice) : undefined
  lastPriceSnapshotKey = form.priceKey
  finishBatchFormCatalogRestore(restoreVersion, provider)
}

async function startBatch(): Promise<void> {
  if (starting.value) return
  if (!settingsReady.value
    || settingsSaving.value
    || servicesLoading.value
    || !catalogReady.value) {
    ElMessage.info('目录正在加载或校验，请稍候')
    return
  }
  if (activeBatchLoading.value) {
    ElMessage.info('正在检查已有任务，请稍候')
    return
  }
  if (currentBatchActive.value) {
    ElMessage.warning(`任务 ${currentBatch.value?.id ?? ''} 正在运行，请先停止当前任务`)
    return
  }
  if (pricesLoading.value) {
    ElMessage.warning('价格正在刷新，请稍候')
    return
  }
  starting.value = true
  try {
    const valid = await batchFormRef.value?.validate().catch(() => false)
    if (!valid) return
    const offer = selectedPrice.value
    if (!offer) {
      ElMessage.warning('价格已变化，请重新选择')
      return
    }
    const offerProvider = smsProvider.value === 'smsbower' ? offer.provider : undefined

    const payload = await api.createBatch({
      sms_provider: smsProvider.value,
      service: form.service,
      service_name: selectedService.value?.label,
      country: form.country,
      country_name: selectedCountry.value?.label,
      price: offer.price,
      max_price: offer.price === undefined ? undefined : String(offer.price),
      currency: currentSMSProviderProfile.value.priceCurrencyCode || undefined,
      provider: offerProvider,
      provider_ids: offerProvider !== undefined
        && Number.isFinite(Number(offerProvider))
        && Number(offerProvider) > 0
        ? [Number(offerProvider)]
        : undefined,
      quantity: form.quantity,
      pin: form.pin,
      proxy_pool: form.proxy.trim() || undefined,
    })
    const batch = normalizeBatch(payload)
    if (!batch.id) throw new Error('服务未返回任务 ID')

    dashboardGeneration += 1
    currentBatch.value = batch
    currentBatchId.value = batch.id
    activations.value = []
    connectionError.value = ''
    ElMessage.success('任务已启动')
    startPolling()
    await refreshDashboard(true)
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    starting.value = false
  }
}

async function refreshDashboard(silent = true): Promise<void> {
  if (refreshing.value) return
  refreshing.value = true
  const requestBatchId = currentBatchId.value
  const requestGeneration = dashboardGeneration

  try {
    // The batch detail response already includes its activation views. Avoid a
    // second 500-row request (and a second verification-code lookup per row)
    // on every two-second refresh.
    const payload = requestBatchId
      ? await api.getBatch(requestBatchId)
      : await api.getActivations()
    if (requestGeneration !== dashboardGeneration || requestBatchId !== currentBatchId.value) return

    if (requestBatchId) {
      currentBatch.value = normalizeBatch(payload)
    }
    activations.value = normalizeActivations(payload)
    connectionError.value = ''
  } catch (error) {
    if (requestGeneration !== dashboardGeneration || requestBatchId !== currentBatchId.value) return
    if (requestBatchId && error instanceof ApiError && error.status === 404) {
      dashboardGeneration += 1
      currentBatchId.value = ''
      currentBatch.value = null
      activations.value = []
      return
    }
    connectionError.value = friendlyError(error)
    if (!silent) ElMessage.error(connectionError.value)
  } finally {
    refreshing.value = false
  }
}

async function stopCurrentBatch(): Promise<void> {
  const batch = currentBatch.value
  if (!batch || !canStopCurrentBatch.value || stoppingBatch.value) return

  try {
    await ElMessageBox.confirm(
      `确定停止任务 ${batch.id} 吗？停止后将不再购买新号码，未完成号码会被取消并从列表移除。此操作不可撤销。`,
      '确认停止任务',
      {
        confirmButtonText: '停止任务',
        cancelButtonText: '继续执行',
        type: 'warning',
      },
    )
  } catch {
    return
  }

  if (stoppingBatch.value) return
  stoppingBatch.value = true
  try {
    const stoppedBatch = normalizeBatch(await api.stopBatch(batch.id))
    if (!stoppedBatch.id) throw new Error('服务未返回停止后的任务信息')
    if (currentBatchId.value !== batch.id) return

    dashboardGeneration += 1
    currentBatch.value = stoppedBatch
    currentBatchId.value = stoppedBatch.id
    activations.value = []
    connectionError.value = ''
    ElMessage.success('停止请求已提交，未完成号码正在取消')
    await refreshDashboard(true)
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    stoppingBatch.value = false
  }
}

function loginStatusForActivation(activation: ActivationView): GoPayLoginStatusView | undefined {
  return findAccountLoginStatusByPhone(accountLoginStatusesByPhone.value, activation.phone)
}

async function refreshAccountLoginStatuses(): Promise<void> {
  if (disposed || loginStatusRefreshing.value) return
  loginStatusRefreshing.value = true
  const controller = new AbortController()
  loginStatusAbortController = controller
  const timeoutTimer = window.setTimeout(() => {
    controller.abort()
  }, LOGIN_STATUS_REQUEST_TIMEOUT_MS)
  try {
    const payload = await api.getAccountLoginStatuses(controller.signal)
    if (disposed || controller.signal.aborted || loginStatusAbortController !== controller) return
    const nextStatuses = normalizeAccountLoginStatuses(payload)
    accountLoginStatuses.value = nextStatuses
  } catch {
    // Keep the last successful result visible; the next five-second poll retries automatically.
  } finally {
    window.clearTimeout(timeoutTimer)
    const isCurrentRequest = loginStatusAbortController === controller
    if (isCurrentRequest) loginStatusAbortController = undefined
    if (!disposed && isCurrentRequest) {
      loginStatusRefreshing.value = false
    }
  }
}

function startPolling(): void {
  if (disposed || pollTimer !== undefined) return
  pollTimer = window.setInterval(() => void refreshDashboard(true), POLL_INTERVAL_MS)
}

function startLoginStatusPolling(): void {
  if (disposed || loginStatusPollTimer !== undefined) return
  loginStatusPollTimer = window.setInterval(
    () => void refreshAccountLoginStatuses(),
    LOGIN_STATUS_POLL_INTERVAL_MS,
  )
}

function startClock(): void {
  if (clockTimer !== undefined) return
  countdownNow.value = Date.now()
  clockTimer = window.setInterval(() => {
    countdownNow.value = Date.now()
  }, CLOCK_INTERVAL_MS)
}

async function markSuccess(activation: ActivationView): Promise<void> {
  actionBusy[activation.id] = true
  try {
    await api.markSuccess(activation.id)
    ElMessage.success(`${activation.phone} 已标记成功`)
    await refreshDashboard(true)
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    actionBusy[activation.id] = false
  }
}

async function deleteActivation(activation: ActivationView): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `删除号码 ${activation.phone}？系统会取消该号码并从列表移除。`,
      '确认删除',
      {
        confirmButtonText: '删除号码',
        cancelButtonText: '返回',
        type: 'warning',
      },
    )
  } catch {
    return
  }

  actionBusy[activation.id] = true
  try {
    await api.deleteActivation(activation.id)
    activations.value = activations.value.filter((item) => item.id !== activation.id)
    ElMessage.success(`${activation.phone} 已删除`)
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    actionBusy[activation.id] = false
  }
}

onMounted(async () => {
  disposed = false
  startClock()
  window.addEventListener('online', handleBrowserOnline)
  window.addEventListener('pagehide', flushBatchDraftOnPageHide)
  if (!legacyClientPersistenceCleared) {
    legacyClientPersistenceCleared = clearLegacyClientPersistence()
    if (!legacyClientPersistenceCleared) {
      ElMessage.warning('浏览器中的旧版明文缓存清理失败；请允许站点存储访问后刷新页面重试')
    }
  }

  const persistedBatchDraft = await loadBatchFormDraft()
  if (disposed) return
  if (persistedBatchDraft) activateBatchDraftPersistence(persistedBatchDraft)
  const startupBatchDraft = persistedBatchDraft ?? currentBatchFormDraft()

  startLoginStatusPolling()
  await Promise.all([
    loadSettings(),
    restoreLatestBatchFromServer(),
    refreshAccountLoginStatuses(),
  ])
  if (disposed) return
  await refreshDashboard(true)
  if (disposed) return
  activeBatchLoading.value = false
  if (settingsReady.value) {
    await restoreBatchFormCatalog(startupBatchDraft)
  } else {
    cancelBatchFormCatalogRestore()
    catalogReady.value = false
  }
  if (disposed) return
  if (!persistedBatchDraft) scheduleBatchDraftLoadRetry()
  startPolling()
})

onBeforeUnmount(() => {
  activeBatchLoading.value = false
  if (batchDraftSaveTimer !== undefined) window.clearTimeout(batchDraftSaveTimer)
  batchDraftSaveTimer = undefined
  if (batchDraftSaveRetryTimer !== undefined) window.clearTimeout(batchDraftSaveRetryTimer)
  batchDraftSaveRetryTimer = undefined
  if (batchDraftLoadRetryTimer !== undefined) window.clearTimeout(batchDraftLoadRetryTimer)
  batchDraftLoadRetryTimer = undefined
  if (batchDraftPersistenceReady) void enqueueBatchDraftSave(currentBatchFormDraft(), true)
  batchDraftPersistenceReady = false
  disposed = true
  window.removeEventListener('online', handleBrowserOnline)
  window.removeEventListener('pagehide', flushBatchDraftOnPageHide)
  loginStatusAbortController?.abort()
  loginStatusAbortController = undefined
  loginStatusRefreshing.value = false
  if (pollTimer !== undefined) window.clearInterval(pollTimer)
  if (loginStatusPollTimer !== undefined) window.clearInterval(loginStatusPollTimer)
  if (clockTimer !== undefined) window.clearInterval(clockTimer)
  pollTimer = undefined
  loginStatusPollTimer = undefined
  clockTimer = undefined
})
</script>

<template>
  <div class="app-shell">
    <header class="topbar">
      <div class="brand">
        <div class="brand__mark" aria-hidden="true">
          <span />
          <span />
          <span />
        </div>
        <div>
          <strong>GoPay AutoSMS</strong>
          <span>号码运营工作台</span>
        </div>
      </div>

      <div class="topbar__state">
        <a class="hero-sms-entry" href="/hero-sms" @click.prevent="openHeroSmsPage">HeroSMS 独立购号</a>
        <span class="live-dot" />
        {{ visibleConnectionError ? '连接异常' : '服务运行中' }}
      </div>
    </header>

    <main class="workspace">
      <section class="hero">
        <div>
          <p class="hero__eyebrow">AUTOMATION CONSOLE</p>
          <h1>号码任务与验证码</h1>
          <p>从购买、登录到余额检查与持续收码，所有状态集中在一个页面。</p>
        </div>
        <div class="poll-indicator">
          <span class="poll-indicator__ring" />
          <div>
            <strong>2 秒</strong>
            <span>自动刷新间隔</span>
          </div>
        </div>
      </section>

      <el-alert
        v-if="visibleConnectionError"
        class="connection-alert"
        :title="visibleConnectionError"
        type="error"
        show-icon
        :closable="false"
      >
        <template #default>
          请检查服务状态或 {{ currentSMSProviderProfile.displayName }} 配置，页面会在后台继续尝试连接。
        </template>
      </el-alert>

      <section v-if="successfulActivations.length" class="successful-rail" aria-labelledby="successful-rail-title">
        <header class="successful-rail__head">
          <div>
            <h2 id="successful-rail-title">成功号码</h2>
            <small>完整号码与任务信息集中显示</small>
          </div>
          <span>当前显示 {{ successfulActivations.length }} 个成功号码</span>
        </header>
        <div class="successful-rail__track">
          <ActivationCard
            v-for="activation in successfulActivations"
            :key="activation.id"
            :activation="activation"
            :login-status="loginStatusForActivation(activation)"
            :busy="actionBusy[activation.id]"
            :now-ms="countdownNow"
            @success="markSuccess"
            @delete="deleteActivation"
          />
        </div>
      </section>

      <section class="control-grid">
        <el-card class="panel settings-panel" shadow="never" v-loading="settingsLoading">
          <template #header>
            <div class="panel-heading">
              <div>
                <span class="step-index">01</span>
                <div>
                  <h2>{{ currentSMSProviderProfile.displayName }} 配置</h2>
                  <p>凭据加密保存到服务端数据库，浏览器不保留明文</p>
                </div>
              </div>
              <el-tag v-if="settingsReady" type="success" effect="plain" round>已配置</el-tag>
              <el-tag v-else type="warning" effect="plain" round>待配置</el-tag>
            </div>
          </template>

          <el-form label-position="top" @submit.prevent="saveSettings">
            <el-form-item label="短信平台">
              <el-select
                v-model="smsProvider"
                :disabled="settingsSaving || starting"
                placeholder="选择短信平台"
                @change="handleSMSProviderChange"
              >
                <el-option
                  v-for="provider in SMS_PROVIDERS"
                  :key="provider.value"
                  :label="provider.displayName"
                  :value="provider.value"
                />
              </el-select>
            </el-form-item>
            <el-form-item label="API Key">
              <el-input
                v-model="settings.apiKey"
                type="password"
                show-password
                clearable
                :disabled="settingsSaving"
                autocomplete="off"
                :placeholder="currentSMSProviderProfile.apiKeyPlaceholder"
              />
              <div class="field-hint">{{ currentSMSProviderProfile.apiKeyHint }}</div>
            </el-form-item>
            <el-button
              class="full-button"
              type="primary"
              native-type="submit"
              :loading="settingsSaving"
            >
              保存并加载目录
            </el-button>
          </el-form>
        </el-card>

        <el-card class="panel batch-panel" shadow="never">
          <template #header>
            <div class="panel-heading">
              <div>
                <span class="step-index">02</span>
                <div>
                  <h2>创建购买任务</h2>
                  <p>依次选择服务、国家与价格</p>
                </div>
              </div>
            </div>
          </template>

          <el-form
            ref="batchFormRef"
            :model="form"
            :rules="rules"
            label-position="top"
            @submit.prevent="startBatch"
          >
            <div class="form-row form-row--catalog">
              <el-form-item label="服务" prop="service">
                <el-select
                  v-model="form.service"
                  filterable
                  clearable
                  :loading="servicesLoading"
                  :disabled="!settingsReady || settingsSaving || servicesLoading"
                  placeholder="选择服务"
                  @change="handleServiceChange"
                >
                  <el-option
                    v-for="item in services"
                    :key="item.key"
                    :label="item.label"
                    :value="item.value"
                  />
                </el-select>
              </el-form-item>
              <el-form-item label="国家" prop="country">
                <el-select
                  v-model="form.country"
                  filterable
                  clearable
                  :filter-method="filterCountries"
                  :loading="countriesLoading"
                  :disabled="!form.service || settingsSaving || servicesLoading"
                  placeholder="输入国家或代码（如 US）"
                  @change="handleCountryChange"
                  @clear="resetCountryQuery"
                  @keydown.esc.capture="resetCountryQuery"
                  @visible-change="handleCountryDropdownVisible"
                >
                  <el-option
                    v-for="item in filteredCountries"
                    :key="item.key"
                    :label="item.label"
                    :value="item.value"
                  />
                </el-select>
              </el-form-item>
              <el-form-item :label="priceFieldLabel" prop="priceKey">
                <div class="price-field-content">
                  <div class="price-picker">
                    <el-select
                      v-model="form.priceKey"
                      filterable
                      clearable
                      :loading="pricesLoading"
                      :disabled="!form.country || settingsSaving || servicesLoading || pricesLoading"
                      placeholder="选择价格"
                      @change="handlePriceChange"
                    >
                      <el-option
                        v-for="item in prices"
                        :key="item.key"
                        :label="priceOptionLabel(item)"
                        :value="item.key"
                        :disabled="item.price === undefined || (item.stock !== undefined && item.stock <= 0)"
                      >
                        <div class="price-option">
                          <span class="price-option__label">{{ priceOptionDetails(item) }}</span>
                          <span
                            v-if="supportsPriceTiers && item.tier"
                            class="price-tier"
                            :class="priceTierClass(item.tier)"
                            :title="priceTierTitle(item)"
                          >
                            {{ item.tier }}
                          </span>
                        </div>
                      </el-option>
                    </el-select>
                    <el-button
                      class="price-refresh-button"
                      native-type="button"
                      plain
                      aria-label="刷新价格"
                      title="重新获取当前服务和国家的价格"
                      :loading="pricesLoading"
                      :disabled="!form.country || settingsSaving || servicesLoading || pricesLoading"
                      @click="refreshPrices"
                    >
                      刷新价格
                    </el-button>
                  </div>
                  <div
                    v-if="supportsPriceTiers"
                    class="price-tier-legend"
                    aria-label="供应商等级与价格档位说明"
                  >
                    <span>数据源等级优先 · 缺失时显示价格档位（派生）</span>
                    <span class="price-tier price-tier--bronze">Bronze</span>
                    <span class="price-tier price-tier--silver">Silver</span>
                    <span class="price-tier price-tier--gold">Gold</span>
                  </div>
                  <div v-else class="field-hint">
                    HeroSMS 价格接口未返回币种；报价数值按你的 HeroSMS 账户币种理解。
                  </div>
                </div>
              </el-form-item>
            </div>

            <div class="form-row form-row--details">
              <el-form-item label="计划购买数量" prop="quantity">
                <el-input-number v-model="form.quantity" :min="1" :max="100" controls-position="right" />
                <div class="field-hint">
                  仅成功完成的号码计入数量；失败时会自动补购，直到成功数达到计划数量。
                </div>
              </el-form-item>
              <el-form-item label="设置 PIN" prop="pin">
                <el-input
                  :model-value="form.pin"
                  inputmode="numeric"
                  maxlength="6"
                  show-word-limit
                  autocomplete="new-password"
                  placeholder="6 位数字"
                  @update:model-value="sanitizePin"
                />
              </el-form-item>
            </div>

            <el-form-item class="proxy-pool-field" label="GoPay 代理池（可选，每行一个地址；重复项保留）" prop="proxy">
              <el-input
                v-model="form.proxy"
                type="textarea"
                :rows="5"
                resize="vertical"
                :placeholder="proxyPlaceholder"
              />
              <div class="field-hint">
                支持 HTTP/HTTPS 与 SOCKS5；未写协议默认 HTTP。已使用或预检失败的地址会移出池，多号码并发不会重复分配。
                <span v-if="proxyInputCount">当前输入 {{ proxyInputCount }} 条（重复项不合并）</span>
              </div>
            </el-form-item>

            <div class="batch-submit-row">
              <div class="selection-summary">
                <span>预计单价</span>
                <strong>
                  {{ selectedPrice?.price === undefined
                    ? '—'
                    : [
                      selectedPrice.price,
                      currentSMSProviderProfile.priceCurrencyLabel,
                    ].filter((value) => value !== '').join(' ') }}
                </strong>
                <span
                  v-if="supportsPriceTiers && selectedPrice?.tier"
                  class="price-tier"
                  :class="priceTierClass(selectedPrice.tier)"
                  :title="priceTierTitle(selectedPrice)"
                >
                  {{ selectedPrice.tier }}
                </span>
              </div>
              <el-button
                class="start-button"
                type="primary"
                size="large"
                native-type="submit"
                :loading="starting"
                :disabled="!settingsReady || settingsSaving || !catalogReady || pricesLoading || activeBatchLoading || currentBatchActive"
              >
                启动任务
              </el-button>
              <span v-if="currentBatchActive" class="field-hint">
                当前任务仍在运行，请先停止当前任务后再启动新任务。
              </span>
            </div>
          </el-form>
        </el-card>
      </section>

      <section v-if="currentBatch" class="batch-status-panel">
        <div class="batch-status-panel__head">
          <div class="batch-status-panel__title">
            <span class="section-kicker">CURRENT TASK</span>
            <h2>任务 ID：{{ currentBatch.id }}</h2>
          </div>
          <div class="batch-status-panel__actions">
            <el-tag :type="currentBatchMeta.type" effect="light" round>
              <span v-if="currentBatchMeta.active" class="status-pulse" />
              {{ currentBatchMeta.label }}
            </el-tag>
            <el-button
              v-if="canStopCurrentBatch"
              class="batch-stop-button"
              type="danger"
              plain
              :loading="stoppingBatch"
              :disabled="stoppingBatch"
              @click="stopCurrentBatch"
            >
              停止任务
            </el-button>
          </div>
        </div>

        <div class="batch-metrics">
          <div>
            <span>计划购买</span>
            <strong>{{ currentBatch.total }}</strong>
          </div>
          <div>
            <span>已购买</span>
            <strong>{{ currentBatch.purchased }}</strong>
          </div>
          <div>
            <span>成功</span>
            <strong>{{ currentBatch.successful }}</strong>
          </div>
          <div>
            <span>未成功</span>
            <strong>{{ unsuccessfulCount }}</strong>
          </div>
          <div>
            <span>正在处理</span>
            <strong>{{ currentBatch.inflight }}</strong>
          </div>
          <div>
            <span>代理地址</span>
            <strong>{{ currentBatch.proxyAvailable }}/{{ currentBatch.proxyTotal }}</strong>
          </div>
        </div>

        <el-progress
          :percentage="batchProgress"
          :stroke-width="10"
          :status="currentBatch.status === 'failed' ? 'exception' : undefined"
        />
        <p v-if="currentBatch.message" class="batch-message">{{ currentBatch.message }}</p>
      </section>

      <section class="activation-section">
        <div class="section-heading">
          <div>
            <span class="section-kicker">ACTIVATIONS</span>
            <h2>号码与验证码</h2>
          </div>
          <div class="section-heading__actions">
            <span>{{ remainingActivations.length }} 条其他记录</span>
            <el-button :loading="refreshing" plain @click="refreshDashboard(false)">立即刷新</el-button>
          </div>
        </div>

        <ActivationTable
          v-if="remainingActivations.length"
          :activations="remainingActivations"
          :busy-by-id="actionBusy"
          :get-login-status="loginStatusForActivation"
          :now-ms="countdownNow"
          @success="markSuccess"
          @delete="deleteActivation"
        />

        <el-empty
          v-else
          class="activation-empty"
          :description="successfulActivations.length ? '暂无其他号码记录' : '暂无号码记录，配置参数后启动第一个任务'"
          :image-size="92"
        />
      </section>
    </main>
  </div>
</template>
