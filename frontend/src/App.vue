<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'

import { api, ApiError } from './api'
import { matchesCatalogOption } from './catalogSearch'
import ActivationTable from './components/ActivationTable.vue'
import {
  findAccountLoginStatusByPhone,
  indexAccountLoginStatusesByPhone,
} from './loginStatus'
import { findRefreshedPrice } from './priceOptions'
import {
  batchStatus,
  normalizeAccountLoginStatuses,
  normalizeActivations,
  normalizeBatch,
  normalizeCountries,
  normalizePrices,
  normalizeServices,
  normalizeSettings,
} from './normalizers'
import {
  loadApiKey,
  loadBatchForm,
  mergeClientApiKey,
  safeGetItem,
  safeRemoveItem,
  safeSetItem,
  saveApiKey,
  saveBatchForm,
} from './persistence'
import type { ClientBatchForm, ClientPriceSnapshot, StorageLike } from './persistence'
import type {
  ActivationView,
  BatchView,
  CatalogOption,
  GoPayLoginStatusView,
  PriceOption,
  PriceTier,
  SMSBowerSettings,
} from './types'

const POLL_INTERVAL_MS = 2_000
// The server's default login-status cache is four seconds, so each normal
// five-second browser poll can start a fresh remote probe without a boundary hit.
const LOGIN_STATUS_POLL_INTERVAL_MS = 5_000
const LOGIN_STATUS_REQUEST_TIMEOUT_MS = 20_000
const CLOCK_INTERVAL_MS = 1_000
const ACTIVE_BATCH_KEY = 'gopay-autosms.active-batch'
type ServiceCatalogLoadResult = 'loaded' | 'failed' | 'stale'
const localStorageAccess: StorageLike | undefined = (() => {
  try {
    return globalThis.localStorage
  } catch {
    return undefined
  }
})()
const persistedBatchForm = loadBatchForm()
const persistedPriceSnapshot = persistedBatchForm.priceSnapshot

const settings = reactive<SMSBowerSettings>({
  apiKey: loadApiKey(),
  configured: false,
})
const settingsLoading = ref(false)
const settingsSaving = ref(false)
const settingsReady = computed(() => Boolean(settings.configured))

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
  service: persistedBatchForm.service,
  country: persistedBatchForm.country,
  priceKey: persistedBatchForm.priceKey,
  quantity: persistedBatchForm.quantity,
  pin: persistedBatchForm.pin,
  proxy: persistedBatchForm.proxy,
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
  priceKey: [{ required: true, message: '请选择价格和供应商', trigger: 'change' }],
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
const currentBatch = ref<BatchView | null>(null)
const currentBatchId = ref(safeGetItem(localStorageAccess, ACTIVE_BATCH_KEY) ?? '')
const activeBatchLoading = ref(true)
const activations = ref<ActivationView[]>([])
const actionBusy = reactive<Record<string, boolean>>({})
const countdownNow = ref(Date.now())
const accountLoginStatuses = ref<GoPayLoginStatusView[]>([])
const loginStatusRefreshing = ref(false)
let pollTimer: number | undefined
let loginStatusPollTimer: number | undefined
let clockTimer: number | undefined
let loginStatusAbortController: AbortController | undefined
let disposed = false
let restoringBatchForm = true
let restoringCatalogDraft: ClientBatchForm | undefined = persistedBatchForm
let catalogRestoreVersion = 0
let catalogActionVersion = 0
let serviceRequestVersion = 0
let countryRequestVersion = 0
let priceRequestVersion = 0
let dashboardGeneration = 0
let lastPriceSnapshot = persistedPriceSnapshot
let lastPriceSnapshotKey = persistedBatchForm.priceKey

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
const unsuccessfulCount = computed(() => {
  const batch = currentBatch.value
  if (!batch) return 0
  return Math.max(0, batch.purchased - batch.successful - batch.inflight)
})
const batchProgress = computed(() => {
  const batch = currentBatch.value
  if (!batch || batch.total < 1) return 0
  return Math.min(100, Math.round((batch.purchased / batch.total) * 100))
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

function persistBatchFormDraft(): ClientBatchForm {
  const draft = currentBatchFormDraft()
  saveBatchForm(draft)
  return draft
}

function draftForCatalogRestore(): ClientBatchForm {
  if (!restoringBatchForm || !restoringCatalogDraft) return persistBatchFormDraft()

  const draft: ClientBatchForm = {
    ...restoringCatalogDraft,
    quantity: form.quantity,
    pin: form.pin,
    proxy: form.proxy,
  }
  saveBatchForm(draft)
  return draft
}

function cancelBatchFormCatalogRestore(): void {
  catalogRestoreVersion += 1
  catalogActionVersion += 1
  restoringBatchForm = false
  restoringCatalogDraft = undefined
}

function finishBatchFormCatalogRestore(restoreVersion: number, ready = true): void {
  if (restoreVersion !== catalogRestoreVersion) return
  restoringBatchForm = false
  restoringCatalogDraft = undefined
  catalogReady.value = ready
  persistBatchFormDraft()
}

watch(() => settings.apiKey, (apiKey) => {
  if (apiKey.trim() || !settings.configured) saveApiKey(apiKey)
})

watch(form, () => {
  if (restoringBatchForm) {
    draftForCatalogRestore()
    return
  }
  persistBatchFormDraft()
}, { deep: true })

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

async function restoreActiveBatchFromServer(): Promise<void> {
  try {
    const payload = await api.getBatches()
    const root = payload && typeof payload === 'object' && !Array.isArray(payload)
      ? payload as Record<string, unknown>
      : undefined
    const rawBatches = Array.isArray(root?.batches)
      ? root.batches
      : Array.isArray(payload) ? payload : []
    const activeBatch = rawBatches
      .map((item) => normalizeBatch(item))
      .find((batch) => batch.id && (batch.status === 'pending' || batch.status === 'running'))
    if (!activeBatch || disposed) return

    if (currentBatchId.value !== activeBatch.id || currentBatch.value?.status !== activeBatch.status) {
      dashboardGeneration += 1
      currentBatchId.value = activeBatch.id
      currentBatch.value = activeBatch
      activations.value = []
      safeSetItem(localStorageAccess, ACTIVE_BATCH_KEY, activeBatch.id)
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
  if (!service || !country) return false

  const previousSelection = options.preserveSelection ? selectedPrice.value : undefined
  const hadSelection = Boolean(form.priceKey)
  const requestVersion = ++priceRequestVersion
  pricesLoading.value = true
  try {
    const nextPrices = normalizePrices(await api.getPrices(service, country))
    if (
      requestVersion !== priceRequestVersion
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
        ElMessage.warning('原报价已失效，请重新选择价格和供应商')
      }
    } else if (options.preserveSelection && hadSelection) {
      form.priceKey = ''
      ElMessage.warning('原报价已失效，请重新选择价格和供应商')
    }

    connectionError.value = ''
    if (options.notifySuccess) {
      ElMessage.success(`价格已刷新，共 ${nextPrices.length} 个报价`)
    }
    return true
  } catch (error) {
    if (requestVersion === priceRequestVersion) ElMessage.error(friendlyError(error))
    return false
  } finally {
    if (requestVersion === priceRequestVersion) pricesLoading.value = false
  }
}

async function refreshPrices(): Promise<void> {
  const actionVersion = ++catalogActionVersion
  const service = form.service
  const country = form.country
  catalogReady.value = false
  const loaded = await loadPrices({ preserveSelection: true, notifySuccess: true })
  if (actionVersion === catalogActionVersion
    && service === form.service
    && country === form.country) catalogReady.value = loaded
}

async function loadSettings(): Promise<void> {
  settingsLoading.value = true
  try {
    const payload = await api.getSettings()
    const loaded = normalizeSettings(payload)
    settings.configured = loaded.configured
    // The endpoint deliberately returns a masked key. Keep the locally
    // persisted plaintext in the input instead of replacing it with the mask.
    const clientApiKey = settings.apiKey.trim() || loadApiKey()
    settings.apiKey = mergeClientApiKey(clientApiKey, loaded.apiKey)
  } catch (error) {
    connectionError.value = friendlyError(error)
  } finally {
    settingsLoading.value = false
  }
}

async function saveSettings(): Promise<void> {
  if (settingsSaving.value) return
  if (!settings.apiKey.trim() && !settings.configured) {
    ElMessage.warning('请输入 SMSBower API Key')
    return
  }
  settingsSaving.value = true
  try {
    const apiKey = settings.apiKey.includes('*') ? '' : settings.apiKey.trim()
    await api.saveSettings({
      apiKey,
    })
    if (apiKey) saveApiKey(apiKey)
    connectionError.value = ''
    ElMessage.success('SMSBower 配置已保存')
    await loadSettings()
    if (settings.configured) {
      await restoreBatchFormCatalog(draftForCatalogRestore())
    } else {
      cancelBatchFormCatalogRestore()
      catalogReady.value = false
      persistBatchFormDraft()
    }
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    settingsSaving.value = false
  }
}

async function loadServices(): Promise<ServiceCatalogLoadResult> {
  const requestVersion = ++serviceRequestVersion
  servicesLoading.value = true
  try {
    const nextServices = normalizeServices(await api.getServices())
    if (requestVersion !== serviceRequestVersion || disposed) return 'stale'
    services.value = nextServices
    connectionError.value = ''
    return 'loaded'
  } catch (error) {
    if (requestVersion !== serviceRequestVersion || disposed) return 'stale'
    services.value = []
    connectionError.value = friendlyError(error)
    return 'failed'
  } finally {
    if (requestVersion === serviceRequestVersion) servicesLoading.value = false
  }
}

async function loadCountriesForCurrentService(): Promise<boolean> {
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
    const nextCountries = normalizeCountries(await api.getCountries(service))
    if (requestVersion !== countryRequestVersion || service !== form.service) return false
    countries.value = nextCountries
    connectionError.value = ''
    return true
  } catch (error) {
    if (requestVersion === countryRequestVersion) ElMessage.error(friendlyError(error))
    return false
  } finally {
    if (requestVersion === countryRequestVersion) countriesLoading.value = false
  }
}

async function handleServiceChange(): Promise<void> {
  catalogReady.value = false
  cancelBatchFormCatalogRestore()
  const actionVersion = catalogActionVersion
  const loaded = await loadCountriesForCurrentService()
  if (actionVersion === catalogActionVersion) catalogReady.value = loaded
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
  const loaded = await loadPricesForCurrentCountry()
  if (actionVersion === catalogActionVersion) catalogReady.value = loaded
}

function handlePriceChange(): void {
  cancelBatchFormCatalogRestore()
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
  if (!settings.configured) return
  const restoreVersion = ++catalogRestoreVersion
  catalogActionVersion += 1
  restoringBatchForm = true
  restoringCatalogDraft = draft
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
  if (disposed || restoreVersion !== catalogRestoreVersion) return
  if (servicesLoaded === 'stale') return
  if (servicesLoaded === 'failed') {
    form.service = draft.service
    form.country = draft.country
    form.priceKey = draft.priceKey
    finishBatchFormCatalogRestore(restoreVersion, false)
    return
  }

  const service = draft.service
  const country = draft.country
  form.service = services.value.some((item) => item.value === service) ? service : ''
  if (!form.service) {
    form.country = ''
    form.priceKey = ''
    finishBatchFormCatalogRestore(restoreVersion)
    return
  }

  const countriesLoaded = await loadCountriesForCurrentService()
  if (disposed || restoreVersion !== catalogRestoreVersion) return
  if (!countriesLoaded) {
    form.country = country
    form.priceKey = draft.priceKey
    finishBatchFormCatalogRestore(restoreVersion, false)
    return
  }
  form.country = countries.value.some((item) => item.value === country) ? country : ''
  if (!form.country) {
    finishBatchFormCatalogRestore(restoreVersion)
    return
  }

  const pricesLoaded = await loadPricesForCurrentCountry()
  if (disposed || restoreVersion !== catalogRestoreVersion) return
  if (!pricesLoaded) {
    form.priceKey = draft.priceKey
    finishBatchFormCatalogRestore(restoreVersion, false)
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
  finishBatchFormCatalogRestore(restoreVersion)
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

    const payload = await api.createBatch({
      service: form.service,
      service_name: selectedService.value?.label,
      country: form.country,
      country_name: selectedCountry.value?.label,
      price: offer.price,
      max_price: offer.price === undefined ? undefined : String(offer.price),
      provider: offer.provider,
      provider_ids: offer.provider !== undefined
        && Number.isFinite(Number(offer.provider))
        && Number(offer.provider) > 0
        ? [Number(offer.provider)]
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
    safeSetItem(localStorageAccess, ACTIVE_BATCH_KEY, batch.id)
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
    const requests: Promise<unknown>[] = []
    if (requestBatchId) requests.push(api.getBatch(requestBatchId))
    requests.push(api.getActivations(requestBatchId || undefined))
    const results = await Promise.allSettled(requests)
    if (requestGeneration !== dashboardGeneration || requestBatchId !== currentBatchId.value) return

    let activationPayload: unknown
    let batchPayload: unknown
    let partialError: unknown
    if (requestBatchId) {
      const batchResult = results[0]
      const activationResult = results[1]
      if (batchResult?.status === 'fulfilled') {
        batchPayload = batchResult.value
        currentBatch.value = normalizeBatch(batchResult.value)
      } else if (batchResult?.status === 'rejected') {
        if (batchResult.reason instanceof ApiError && batchResult.reason.status === 404) {
          dashboardGeneration += 1
          currentBatchId.value = ''
          currentBatch.value = null
          activations.value = []
          safeRemoveItem(localStorageAccess, ACTIVE_BATCH_KEY)
          return
        } else {
          partialError = batchResult.reason
        }
      }
      if (activationResult?.status === 'fulfilled') activationPayload = activationResult.value
      if (activationResult?.status === 'rejected' && batchPayload !== undefined) {
        activationPayload = batchPayload
      }
      if (batchResult?.status === 'rejected' && activationResult?.status === 'rejected') {
        throw batchResult.reason
      }
    } else {
      const activationResult = results[0]
      if (activationResult?.status === 'fulfilled') activationPayload = activationResult.value
      else if (activationResult?.status === 'rejected') throw activationResult.reason
    }

    if (activationPayload !== undefined) activations.value = normalizeActivations(activationPayload)
    if (partialError) throw partialError
    connectionError.value = ''
  } catch (error) {
    if (requestGeneration !== dashboardGeneration || requestBatchId !== currentBatchId.value) return
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
    safeSetItem(localStorageAccess, ACTIVE_BATCH_KEY, stoppedBatch.id)
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
      `删除号码 ${activation.phone}？系统会取消该号码，并保留历史去重记录。`,
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
  startLoginStatusPolling()
  await Promise.all([
    loadSettings(),
    restoreActiveBatchFromServer(),
    refreshAccountLoginStatuses(),
  ])
  if (disposed) return
  await refreshDashboard(true)
  if (disposed) return
  activeBatchLoading.value = false
  if (settingsReady.value) {
    await restoreBatchFormCatalog(persistedBatchForm)
  } else {
    cancelBatchFormCatalogRestore()
    catalogReady.value = false
    persistBatchFormDraft()
  }
  if (disposed) return
  startPolling()
})

onBeforeUnmount(() => {
  disposed = true
  activeBatchLoading.value = false
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
        <span class="live-dot" />
        {{ connectionError ? '连接异常' : '服务运行中' }}
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
        v-if="connectionError"
        class="connection-alert"
        :title="connectionError"
        type="error"
        show-icon
        :closable="false"
      >
        <template #default>
          请检查服务状态或 SMSBower 配置，页面会在后台继续尝试连接。
        </template>
      </el-alert>

      <section class="control-grid">
        <el-card class="panel settings-panel" shadow="never" v-loading="settingsLoading">
          <template #header>
            <div class="panel-heading">
              <div>
                <span class="step-index">01</span>
                <div>
                  <h2>SMSBower 配置</h2>
                  <p>凭据同步保存到服务端，并在此客户端保留输入</p>
                </div>
              </div>
              <el-tag v-if="settingsReady" type="success" effect="plain" round>已配置</el-tag>
              <el-tag v-else type="warning" effect="plain" round>待配置</el-tag>
            </div>
          </template>

          <el-form label-position="top" @submit.prevent="saveSettings">
            <el-form-item label="API Key">
              <el-input
                v-model="settings.apiKey"
                type="password"
                show-password
                clearable
                :disabled="settingsSaving"
                autocomplete="off"
                placeholder="输入 SMSBower API Key"
              />
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
              <el-form-item label="价格 / 供应商" prop="priceKey">
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
                            v-if="item.tier"
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
                  <div class="price-tier-legend" aria-label="供应商等级与价格档位说明">
                    <span>数据源等级优先 · 缺失时显示价格档位（派生）</span>
                    <span class="price-tier price-tier--bronze">Bronze</span>
                    <span class="price-tier price-tier--silver">Silver</span>
                    <span class="price-tier price-tier--gold">Gold</span>
                  </div>
                </div>
              </el-form-item>
            </div>

            <div class="form-row form-row--details">
              <el-form-item label="计划购买数量" prop="quantity">
                <el-input-number v-model="form.quantity" :min="1" :max="100" controls-position="right" />
                <div class="field-hint">
                  每成功购买一个号码即计入数量；号码后续处理未成功时也不会自动补购。
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
                <strong>{{ selectedPrice?.price === undefined ? '—' : `${selectedPrice.price} ₽` }}</strong>
                <span
                  v-if="selectedPrice?.tier"
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
            <span>{{ activations.length }} 条记录</span>
            <el-button :loading="refreshing" plain @click="refreshDashboard(false)">立即刷新</el-button>
          </div>
        </div>

        <ActivationTable
          v-if="activations.length"
          :activations="activations"
          :busy-by-id="actionBusy"
          :get-login-status="loginStatusForActivation"
          :now-ms="countdownNow"
          @success="markSuccess"
          @delete="deleteActivation"
        />

        <el-empty
          v-else
          class="activation-empty"
          description="暂无号码记录，配置参数后启动第一个任务"
          :image-size="92"
        />
      </section>
    </main>
  </div>
</template>
