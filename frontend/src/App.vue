<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'

import { api, ApiError } from './api'
import ActivationCard from './components/ActivationCard.vue'
import {
  activationStatus,
  batchStatus,
  normalizeActivations,
  normalizeBatch,
  normalizeCountries,
  normalizePrices,
  normalizeServices,
  normalizeSettings,
} from './normalizers'
import type {
  ActivationView,
  BatchView,
  CatalogOption,
  PriceOption,
  SMSBowerSettings,
} from './types'

const POLL_INTERVAL_MS = 2_000
const ACTIVE_BATCH_KEY = 'gopay-autosms.active-batch'

const settings = reactive<SMSBowerSettings>({
  apiKey: '',
  configured: false,
})
const settingsLoading = ref(false)
const settingsSaving = ref(false)
const settingsReady = computed(() => Boolean(
  settings.configured || settings.apiKey.trim(),
))

const services = ref<CatalogOption[]>([])
const countries = ref<CatalogOption[]>([])
const prices = ref<PriceOption[]>([])
const servicesLoading = ref(false)
const countriesLoading = ref(false)
const pricesLoading = ref(false)

const batchFormRef = ref<FormInstance>()
const form = reactive({
  service: '',
  country: '',
  priceKey: '',
  quantity: 1,
  pin: '',
  proxy: '',
})

const proxyInputCount = computed(() => form.proxy.split(/\r?\n/).map((item) => item.trim()).filter(Boolean).length)

const rules: FormRules = {
  service: [{ required: true, message: '请选择服务', trigger: 'change' }],
  country: [{ required: true, message: '请选择国家', trigger: 'change' }],
  priceKey: [{ required: true, message: '请选择价格和供应商', trigger: 'change' }],
  quantity: [
    { required: true, message: '请输入购买数量', trigger: 'change' },
    {
      validator: (_rule, value, callback) => {
        if (!Number.isInteger(value) || value < 1 || value > 100) {
          callback(new Error('数量范围为 1–100'))
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
const refreshing = ref(false)
const connectionError = ref('')
const currentBatch = ref<BatchView | null>(null)
const currentBatchId = ref(localStorage.getItem(ACTIVE_BATCH_KEY) ?? '')
const activations = ref<ActivationView[]>([])
const actionBusy = reactive<Record<string, boolean>>({})
let pollTimer: number | undefined

const selectedPrice = computed(() => prices.value.find((item) => item.key === form.priceKey))
const selectedService = computed(() => services.value.find((item) => item.value === form.service))
const selectedCountry = computed(() => countries.value.find((item) => item.value === form.country))
const currentBatchMeta = computed(() => batchStatus(currentBatch.value?.status ?? 'pending'))
const activeActivationCount = computed(
  () => activations.value.filter((item) => !item.finishedAt && activationStatus(item.status).active).length,
)
const unqualifiedCount = computed(() => {
  const disqualified = new Set([
    'pin_required',
    'unregistered',
    'duplicate',
    'zero_rp_used',
    'zero_balance_used',
    'expired',
    'cancelled',
    'failed',
  ])
  return Math.max(
    currentBatch.value?.failed ?? 0,
    activations.value.filter((item) => disqualified.has(item.status)).length,
  )
})
const batchProgress = computed(() => {
  const batch = currentBatch.value
  if (!batch || batch.total < 1) return 0
  return Math.min(100, Math.round((batch.successful / batch.total) * 100))
})

function friendlyError(error: unknown): string {
  if (error instanceof ApiError) return error.message
  return error instanceof Error ? error.message : '发生未知错误'
}

function sanitizePin(value: string): void {
  form.pin = value.replace(/\D/g, '').slice(0, 6)
}

async function loadSettings(): Promise<void> {
  settingsLoading.value = true
  try {
    const payload = await api.getSettings()
    Object.assign(settings, normalizeSettings(payload))
  } catch (error) {
    connectionError.value = friendlyError(error)
  } finally {
    settingsLoading.value = false
  }
}

async function saveSettings(): Promise<void> {
  if (!settings.apiKey.trim() && !settings.configured) {
    ElMessage.warning('请输入 SMSBower API Key')
    return
  }
  settingsSaving.value = true
  try {
    await api.saveSettings({
      apiKey: settings.apiKey.includes('*') ? '' : settings.apiKey.trim(),
    })
    connectionError.value = ''
    ElMessage.success('SMSBower 配置已保存')
    await loadSettings()
    await loadServices()
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    settingsSaving.value = false
  }
}

async function loadServices(): Promise<void> {
  servicesLoading.value = true
  try {
    services.value = normalizeServices(await api.getServices())
    connectionError.value = ''
  } catch (error) {
    services.value = []
    connectionError.value = friendlyError(error)
  } finally {
    servicesLoading.value = false
  }
}

async function handleServiceChange(): Promise<void> {
  form.country = ''
  form.priceKey = ''
  countries.value = []
  prices.value = []
  if (!form.service) return

  countriesLoading.value = true
  try {
    countries.value = normalizeCountries(await api.getCountries(form.service))
    connectionError.value = ''
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    countriesLoading.value = false
  }
}

async function handleCountryChange(): Promise<void> {
  form.priceKey = ''
  prices.value = []
  if (!form.service || !form.country) return

  pricesLoading.value = true
  try {
    prices.value = normalizePrices(await api.getPrices(form.service, form.country))
    connectionError.value = ''
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    pricesLoading.value = false
  }
}

async function startBatch(): Promise<void> {
  const valid = await batchFormRef.value?.validate().catch(() => false)
  if (!valid) return
  const offer = selectedPrice.value
  if (!offer) {
    ElMessage.warning('价格已变化，请重新选择')
    return
  }

  starting.value = true
  try {
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
    if (!batch.id) throw new Error('服务未返回批次 ID')

    currentBatch.value = batch
    currentBatchId.value = batch.id
    localStorage.setItem(ACTIVE_BATCH_KEY, batch.id)
    connectionError.value = ''
    ElMessage.success('批次已启动')
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

  try {
    const requests: Promise<unknown>[] = []
    if (currentBatchId.value) requests.push(api.getBatch(currentBatchId.value))
    requests.push(api.getActivations(currentBatchId.value || undefined))
    const results = await Promise.allSettled(requests)

    let activationPayload: unknown
    let batchPayload: unknown
    let partialError: unknown
    if (currentBatchId.value) {
      const batchResult = results[0]
      const activationResult = results[1]
      if (batchResult?.status === 'fulfilled') {
        batchPayload = batchResult.value
        currentBatch.value = normalizeBatch(batchResult.value)
      } else if (batchResult?.status === 'rejected') {
        if (batchResult.reason instanceof ApiError && batchResult.reason.status === 404) {
          currentBatchId.value = ''
          currentBatch.value = null
          localStorage.removeItem(ACTIVE_BATCH_KEY)
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
    connectionError.value = friendlyError(error)
    if (!silent) ElMessage.error(connectionError.value)
  } finally {
    refreshing.value = false
  }
}

function startPolling(): void {
  if (pollTimer !== undefined) return
  pollTimer = window.setInterval(() => void refreshDashboard(true), POLL_INTERVAL_MS)
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
  await Promise.all([loadSettings(), refreshDashboard(true)])
  if (settingsReady.value) await loadServices()
  startPolling()
})

onBeforeUnmount(() => {
  if (pollTimer !== undefined) window.clearInterval(pollTimer)
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
          <h1>号码批次与验证码</h1>
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
                  <p>凭据仅由后端保存和调用</p>
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
                  <h2>创建购买批次</h2>
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
                  :disabled="!settingsReady"
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
                  :loading="countriesLoading"
                  :disabled="!form.service"
                  placeholder="选择国家"
                  @change="handleCountryChange"
                >
                  <el-option
                    v-for="item in countries"
                    :key="item.key"
                    :label="item.label"
                    :value="item.value"
                  />
                </el-select>
              </el-form-item>
              <el-form-item label="价格 / 供应商" prop="priceKey">
                <el-select
                  v-model="form.priceKey"
                  filterable
                  clearable
                  :loading="pricesLoading"
                  :disabled="!form.country"
                  placeholder="选择价格"
                >
                  <el-option
                    v-for="item in prices"
                    :key="item.key"
                    :label="item.label"
                    :value="item.key"
                    :disabled="item.stock === 0"
                  />
                </el-select>
              </el-form-item>
            </div>

            <div class="form-row form-row--details">
              <el-form-item label="购买数量" prop="quantity">
                <el-input-number v-model="form.quantity" :min="1" :max="100" controls-position="right" />
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
                placeholder="hostname:port:username:password\nsocks5://username:password@host:port\nusername:password@hostname:port\nhostname:port@username:password"
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
              </div>
              <el-button
                class="start-button"
                type="primary"
                size="large"
                native-type="submit"
                :loading="starting"
                :disabled="!settingsReady"
              >
                启动批次
              </el-button>
            </div>
          </el-form>
        </el-card>
      </section>

      <section v-if="currentBatch" class="batch-status-panel">
        <div class="batch-status-panel__head">
          <div>
            <span class="section-kicker">CURRENT BATCH</span>
            <h2>批次 {{ currentBatch.id }}</h2>
          </div>
          <el-tag :type="currentBatchMeta.type" effect="light" round>
            <span v-if="currentBatchMeta.active" class="status-pulse" />
            {{ currentBatchMeta.label }}
          </el-tag>
        </div>

        <div class="batch-metrics">
          <div>
            <span>目标数量</span>
            <strong>{{ currentBatch.total }}</strong>
          </div>
          <div>
            <span>已达标</span>
            <strong>{{ currentBatch.successful }}</strong>
          </div>
          <div>
            <span>购买占用</span>
            <strong>{{ currentBatch.inflight }}</strong>
          </div>
          <div>
            <span>未计入</span>
            <strong>{{ unqualifiedCount }}</strong>
          </div>
          <div>
            <span>正在处理</span>
            <strong>{{ activeActivationCount }}</strong>
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

        <div v-if="activations.length" class="activation-grid">
          <ActivationCard
            v-for="activation in activations"
            :key="activation.id"
            :activation="activation"
            :busy="actionBusy[activation.id]"
            @success="markSuccess"
            @delete="deleteActivation"
          />
        </div>

        <el-empty
          v-else
          class="activation-empty"
          description="暂无号码记录，配置参数后启动第一个批次"
          :image-size="92"
        />
      </section>
    </main>
  </div>
</template>
