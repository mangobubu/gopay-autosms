<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'

import HeroSmsNumberTaskCard from '../components/heroSms/HeroSmsNumberTask.vue'
import { createHeroSmsIdempotencyKey, heroSmsApi, HeroSmsApiError } from '../heroSmsApi'
import {
  heroSmsDurationChoices,
  mergeHeroSmsCatalog,
  mergeHeroSmsTasks,
  normalizeHeroSmsCatalog,
  normalizeHeroSmsTaskEnvelope,
} from '../heroSmsNormalizers'
import type {
  HeroSmsCatalog,
  HeroSmsCatalogFilters,
  HeroSmsNumberTask,
  HeroSmsOffer,
  HeroSmsTaskAction,
} from '../heroSmsTypes'

const TASK_POLL_INTERVAL_MS = 2_000
const CLOCK_INTERVAL_MS = 1_000

const emptyCatalog = (): HeroSmsCatalog => ({
  services: [],
  countries: [],
  verificationTypes: [],
  durations: [],
  offers: [],
})

const formRef = ref<FormInstance>()
const form = reactive({
  service: '',
  country: '',
  verificationType: '',
  durationHours: undefined as number | undefined,
  quantity: 1,
})
const rules: FormRules = {
  service: [{ required: true, message: '请选择服务', trigger: 'change' }],
  country: [{ required: true, message: '请选择国家', trigger: 'change' }],
  verificationType: [{ required: true, message: '请选择验证方式', trigger: 'change' }],
  quantity: [{
    validator: (_rule, value, callback) => {
      if (!Number.isInteger(value) || value < 1 || value > 100) {
        callback(new Error('购买数量范围为 1–100'))
        return
      }
      callback()
    },
    trigger: 'change',
  }],
}

const catalog = ref<HeroSmsCatalog>(emptyCatalog())
const tasks = ref<HeroSmsNumberTask[]>([])
const catalogLoading = ref(false)
const starting = ref(false)
const refreshing = ref(false)
const catalogError = ref('')
const taskError = ref('')
const catalogMessage = ref('')
const actionBusy = reactive<Record<string, boolean>>({})
const nowMs = ref(Date.now())
const serverClockOffsetMs = ref(0)
let catalogVersion = 0
let pollTimer: number | undefined
let clockTimer: number | undefined
let disposed = false
let pendingPurchase: { signature: string; idempotencyKey: string } | undefined

const filteredOffers = computed(() => catalog.value.offers.filter((offer) => (
  (!offer.service || offer.service === form.service)
    && (!offer.country || offer.country === form.country)
    && (!offer.verificationType || offer.verificationType === form.verificationType)
)))
const durationOptions = computed(() => {
  if (form.verificationType === 'call') {
    return [{ key: 'activation', hours: 0, label: '单次接码（约 20 分钟）' }]
  }
  return heroSmsDurationChoices(filteredOffers.value, catalog.value.durations)
})
const matchingOffer = computed<HeroSmsOffer | undefined>(() => filteredOffers.value.find((offer) => (
  form.durationHours === undefined
    || (form.durationHours === 0
      ? offer.productKind === 'activation' || offer.durationHours === undefined || offer.durationHours === 0
      : offer.durationHours === form.durationHours)
)))
const noInventory = computed(() => Boolean(
  matchingOffer.value && (matchingOffer.value.stock === 0 || matchingOffer.value.available === false),
))
const activeTaskCount = computed(() => tasks.value.filter((task) => task.running).length)
const waitingTaskCount = computed(() => tasks.value.filter((task) => (
  task.status === 'waiting_number' || task.status === 'waiting_inventory' || task.status === 'waiting_stock'
)).length)
const codeCount = computed(() => tasks.value.reduce((sum, task) => sum + task.messages.length, 0))
const connectionError = computed(() => catalogError.value || taskError.value)

function friendlyError(error: unknown): string {
  if (error instanceof HeroSmsApiError) return error.message
  return error instanceof Error ? error.message : '发生未知错误'
}

function applyServerClock(serverNow?: string): void {
  if (!serverNow) return
  const timestamp = Date.parse(serverNow)
  if (Number.isFinite(timestamp)) serverClockOffsetMs.value = timestamp - Date.now()
}

function ensureSelection(): void {
  if (!catalog.value.services.some((item) => item.value === form.service)) {
    form.service = catalog.value.services[0]?.value ?? ''
  }
  if (!catalog.value.countries.some((item) => item.value === form.country)) {
    form.country = catalog.value.countries[0]?.value ?? ''
  }
  if (!catalog.value.verificationTypes.some((item) => item.value === form.verificationType)) {
    form.verificationType = catalog.value.verificationTypes[0]?.value ?? ''
  }
  if (durationOptions.value.length === 0) {
    form.durationHours = undefined
  } else if (!durationOptions.value.some((item) => item.hours === form.durationHours)) {
    form.durationHours = durationOptions.value[0]?.hours
  }
}

async function loadCatalog(filters: HeroSmsCatalogFilters = {}, replaceDependent = false): Promise<void> {
  const version = ++catalogVersion
  catalogLoading.value = true
  try {
    const next = normalizeHeroSmsCatalog(await heroSmsApi.getCatalog(filters))
    if (disposed || version !== catalogVersion) return
    if (replaceDependent) {
      catalog.value = {
        services: next.services.length ? next.services : catalog.value.services,
        countries: next.countries,
        verificationTypes: next.verificationTypes,
        durations: next.durations,
        offers: next.offers,
        message: next.message,
      }
    } else {
      catalog.value = mergeHeroSmsCatalog(catalog.value, next)
    }
    catalogMessage.value = next.message ?? ''
    ensureSelection()
    catalogError.value = ''
  } catch (error) {
    if (!disposed && version === catalogVersion) catalogError.value = friendlyError(error)
  } finally {
    if (!disposed && version === catalogVersion) catalogLoading.value = false
  }
}

async function onServiceChange(): Promise<void> {
  form.country = ''
  form.verificationType = ''
  form.durationHours = undefined
  catalog.value = {
    ...catalog.value,
    countries: [],
    verificationTypes: [],
    durations: [],
    offers: [],
  }
  await loadCatalog({ service: form.service }, true)
  if (form.country) {
    form.durationHours = undefined
    await loadCatalog({ service: form.service, country: form.country }, true)
  }
}

async function onCountryChange(): Promise<void> {
  form.verificationType = ''
  form.durationHours = undefined
  await loadCatalog({ service: form.service, country: form.country }, true)
}

async function onVerificationChange(): Promise<void> {
  form.durationHours = undefined
  await loadCatalog({
    service: form.service,
    country: form.country,
    verificationType: form.verificationType,
  }, true)
}

async function onDurationChange(): Promise<void> {
  await loadCatalog({
    service: form.service,
    country: form.country,
    verificationType: form.verificationType,
    durationHours: form.durationHours,
  }, true)
}

async function refreshTasks(showMessage = false): Promise<void> {
  if (disposed || refreshing.value) return
  refreshing.value = true
  try {
    let cursor: string | undefined
    let loadedTasks: HeroSmsNumberTask[] = []
    let latestServerNow: string | undefined
    const seenCursors = new Set<string>()
    do {
      if (disposed) return
      const result = normalizeHeroSmsTaskEnvelope(await heroSmsApi.getTasks(cursor))
      if (disposed) return
      loadedTasks = mergeHeroSmsTasks(loadedTasks, result.tasks)
      latestServerNow = result.serverNow ?? latestServerNow
      cursor = result.nextCursor
      if (cursor && seenCursors.has(cursor)) {
        throw new Error('任务分页游标重复，已停止本次同步')
      }
      if (cursor) seenCursors.add(cursor)
    } while (cursor)
    if (disposed) return
    tasks.value = mergeHeroSmsTasks(tasks.value, loadedTasks)
    applyServerClock(latestServerNow)
    taskError.value = ''
    if (showMessage) ElMessage.success('任务状态已更新')
  } catch (error) {
    if (!disposed) taskError.value = friendlyError(error)
  } finally {
    if (!disposed) refreshing.value = false
  }
}

async function initializeCatalog(): Promise<void> {
  await loadCatalog()
  if (disposed || !form.service) return
  if (catalog.value.countries.length === 0) {
    await loadCatalog({ service: form.service }, true)
  }
  if (!disposed && form.country && catalog.value.verificationTypes.length === 0 && catalog.value.offers.length === 0) {
    await loadCatalog({ service: form.service, country: form.country }, true)
  }
}

async function startPurchase(): Promise<void> {
  if (starting.value) return
  starting.value = true
  try {
    const valid = await formRef.value?.validate().catch(() => false)
    if (!valid) return
    const input = {
      service: form.service,
      country: form.country,
      verificationType: form.verificationType,
      durationHours: form.durationHours,
      quantity: form.quantity,
    }
    const signature = JSON.stringify(input)
    if (!pendingPurchase || pendingPurchase.signature !== signature) {
      pendingPurchase = { signature, idempotencyKey: createHeroSmsIdempotencyKey() }
    }
    const result = normalizeHeroSmsTaskEnvelope(await heroSmsApi.createTasks(
      input,
      pendingPurchase.idempotencyKey,
    ))
    pendingPurchase = undefined
    tasks.value = mergeHeroSmsTasks(tasks.value, result.tasks)
    applyServerClock(result.serverNow)
    taskError.value = ''
    ElMessage.success(result.tasks.length
      ? `已追加 ${result.tasks.length} 个独立号码任务`
      : '购买请求已提交，正在同步任务')
    if (result.tasks.length === 0) await refreshTasks()
  } catch (error) {
    taskError.value = friendlyError(error)
    ElMessage.error(taskError.value)
  } finally {
    starting.value = false
  }
}

async function runTaskAction(task: HeroSmsNumberTask, action: HeroSmsTaskAction): Promise<void> {
  if (actionBusy[task.id]) return
  actionBusy[task.id] = true
  const prompts: Partial<Record<HeroSmsTaskAction, { title: string; message: string; confirm: string }>> = {
    stop: {
      title: '停止购买任务',
      message: '停止后将不再为这个任务尝试购买号码，其他任务不会受到影响。',
      confirm: '停止购买',
    },
    cancel: {
      title: '申请退款',
      message: '将停止这个号码并申请退款，最终退款资格和金额由服务端确认。',
      confirm: '申请退款',
    },
    settle: {
      title: '停止并结算',
      message: '结算后这个号码将不再接收验证码，且通常不可退款。',
      confirm: '停止并结算',
    },
  }
  const prompt = prompts[action]
  if (prompt) {
    try {
      await ElMessageBox.confirm(prompt.message, prompt.title, {
        confirmButtonText: prompt.confirm,
        cancelButtonText: '返回',
        type: action === 'settle' ? 'warning' : 'info',
      })
    } catch {
      actionBusy[task.id] = false
      return
    }
  }

  try {
    const result = normalizeHeroSmsTaskEnvelope(await heroSmsApi.actOnTask(task.id, action))
    tasks.value = mergeHeroSmsTasks(tasks.value, result.tasks)
    applyServerClock(result.serverNow)
    ElMessage.success(action === 'start' ? '任务已继续' : '操作已提交')
    if (result.tasks.length === 0) await refreshTasks()
  } catch (error) {
    ElMessage.error(friendlyError(error))
  } finally {
    actionBusy[task.id] = false
  }
}

onMounted(async () => {
  await Promise.all([initializeCatalog(), refreshTasks()])
  if (disposed) return
  pollTimer = window.setInterval(() => void refreshTasks(), TASK_POLL_INTERVAL_MS)
  clockTimer = window.setInterval(() => {
    nowMs.value = Date.now() + serverClockOffsetMs.value
  }, CLOCK_INTERVAL_MS)
})

onBeforeUnmount(() => {
  disposed = true
  if (pollTimer !== undefined) window.clearInterval(pollTimer)
  if (clockTimer !== undefined) window.clearInterval(clockTimer)
})
</script>

<template>
  <div class="hero-sms-page">
    <header class="hero-sms-topbar">
      <a class="hero-sms-brand" href="/">
        <span class="hero-sms-brand__mark" aria-hidden="true">H</span>
        <span>
          <strong>HeroSMS</strong>
          <small>号码与验证码</small>
        </span>
      </a>
      <div class="hero-sms-sync" :class="{ 'is-refreshing': refreshing }">
        <span aria-hidden="true" />
        {{ refreshing ? '正在同步' : 'Webhook 优先 · 自动同步' }}
      </div>
    </header>

    <main class="hero-sms-workspace">
      <section class="hero-sms-intro">
        <div>
          <p class="hero-sms-kicker">INDEPENDENT NUMBER TASKS</p>
          <h1>购买号码，持续接收验证码</h1>
          <p>每个号码都是独立任务。新任务只会追加，已有号码会一直运行到有效期结束或手动停止结算。</p>
        </div>
        <a href="/">返回主控制台</a>
      </section>

      <el-alert
        v-if="connectionError"
        class="hero-sms-error"
        :title="connectionError"
        type="error"
        :closable="false"
        show-icon
      />

      <div class="hero-sms-layout">
        <aside class="hero-sms-purchase-panel">
          <div class="hero-sms-panel-heading">
            <div>
              <span>创建新任务</span>
              <h2>购买设置</h2>
            </div>
            <el-tag type="primary" effect="light" round>HeroSMS</el-tag>
          </div>

          <el-form
            ref="formRef"
            :model="form"
            :rules="rules"
            label-position="top"
            :disabled="catalogLoading || starting"
            @submit.prevent="startPurchase"
          >
            <el-form-item label="服务" prop="service">
              <el-select
                v-model="form.service"
                filterable
                placeholder="选择接码服务"
                no-data-text="暂无服务"
                @change="onServiceChange"
              >
                <el-option
                  v-for="item in catalog.services"
                  :key="item.key"
                  :label="item.label"
                  :value="item.value"
                />
              </el-select>
            </el-form-item>

            <el-form-item label="国家 / 地区" prop="country">
              <el-select
                v-model="form.country"
                filterable
                placeholder="选择国家或输入名称搜索"
                no-data-text="请先选择服务"
                @change="onCountryChange"
              >
                <el-option
                  v-for="item in catalog.countries"
                  :key="item.key"
                  :label="item.label"
                  :value="item.value"
                />
              </el-select>
            </el-form-item>

            <el-form-item label="验证方式" prop="verificationType">
              <el-radio-group v-if="catalog.verificationTypes.length" v-model="form.verificationType" @change="onVerificationChange">
                <el-radio-button
                  v-for="item in catalog.verificationTypes"
                  :key="item.key"
                  :value="item.value"
                >
                  {{ item.label }}
                </el-radio-button>
              </el-radio-group>
              <div v-else class="hero-sms-field-empty">选择服务与国家后显示可用方式</div>
            </el-form-item>

            <el-form-item v-if="durationOptions.length > 1" label="号码时长" prop="durationHours">
              <el-select v-model="form.durationHours" placeholder="选择时长" @change="onDurationChange">
                <el-option
                  v-for="item in durationOptions"
                  :key="item.key"
                  :label="item.label"
                  :value="item.hours"
                />
              </el-select>
            </el-form-item>

            <div class="hero-sms-form-row">
              <el-form-item label="购买数量" prop="quantity">
                <el-input-number v-model="form.quantity" :min="1" :max="100" controls-position="right" />
              </el-form-item>
              <div class="hero-sms-price-summary">
                <span>参考单价</span>
                <strong v-if="matchingOffer?.price !== undefined">
                  {{ matchingOffer.price }} {{ matchingOffer.currency || '' }}
                </strong>
                <strong v-else>购买时确认</strong>
              </div>
            </div>

            <el-alert
              v-if="noInventory"
              title="该服务当前暂无可用号码。启动后会创建独立任务，并持续尝试购买，直到你手动停止。"
              type="warning"
              :closable="false"
              show-icon
            />
            <el-alert
              v-else-if="catalogMessage"
              :title="catalogMessage"
              type="info"
              :closable="false"
              show-icon
            />

            <el-button
              class="hero-sms-start"
              type="primary"
              native-type="submit"
              :loading="starting"
              :disabled="!form.service || !form.country || !form.verificationType"
            >
              {{ noInventory ? '开始持续购买' : '开始购买' }}
            </el-button>
          </el-form>

          <div class="hero-sms-form-note">
            <strong>任务互不影响</strong>
            <p>数量大于 1 时，每个号码都会创建为一个独立任务。停止某个号码不会停止其他号码。</p>
          </div>
        </aside>

        <section class="hero-sms-task-area">
          <div class="hero-sms-task-heading">
            <div>
              <span>号码任务</span>
              <h2>购买与接收进度</h2>
            </div>
            <el-button :loading="refreshing" @click="refreshTasks(true)">立即刷新</el-button>
          </div>

          <div v-if="tasks.length" class="hero-sms-stats">
            <div><span>全部任务</span><strong>{{ tasks.length }}</strong></div>
            <div><span>运行中</span><strong>{{ activeTaskCount }}</strong></div>
            <div><span>等待号码</span><strong>{{ waitingTaskCount }}</strong></div>
            <div><span>已收验证码</span><strong>{{ codeCount }}</strong></div>
          </div>

          <div v-if="tasks.length" class="hero-sms-task-list">
            <HeroSmsNumberTaskCard
              v-for="task in tasks"
              :key="task.id"
              :task="task"
              :now-ms="nowMs"
              :busy="actionBusy[task.id]"
              @action="runTaskAction"
            />
          </div>
          <el-empty v-else description="还没有号码任务">
            <p class="hero-sms-empty-copy">在左侧选择服务并开始购买，新任务会追加显示在这里。</p>
          </el-empty>
        </section>
      </div>
    </main>
  </div>
</template>

<style scoped>
.hero-sms-page {
  min-height: 100vh;
  color: #172235;
  background:
    radial-gradient(circle at 12% 0%, rgba(50, 112, 238, 0.11), transparent 28rem),
    radial-gradient(circle at 90% 18%, rgba(24, 176, 127, 0.08), transparent 25rem),
    #f2f5f9;
}

.hero-sms-topbar {
  position: sticky;
  z-index: 20;
  top: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 68px;
  padding: 0 24px;
  color: #fff;
  border-bottom: 1px solid rgba(255, 255, 255, 0.08);
  background: rgba(13, 23, 40, 0.96);
  backdrop-filter: blur(18px);
}

.hero-sms-brand {
  display: flex;
  align-items: center;
  gap: 11px;
  color: inherit;
  text-decoration: none;
}

.hero-sms-brand__mark {
  display: grid;
  width: 35px;
  height: 35px;
  place-items: center;
  border-radius: 11px;
  background: linear-gradient(145deg, #317bff, #1755ca);
  box-shadow: 0 8px 22px rgba(26, 86, 206, 0.45);
  font-weight: 900;
}

.hero-sms-brand > span:last-child {
  display: flex;
  flex-direction: column;
}

.hero-sms-brand strong { font-size: 15px; }
.hero-sms-brand small { color: #9eabc0; font-size: 10px; }

.hero-sms-sync {
  display: flex;
  align-items: center;
  gap: 8px;
  color: #bdc9db;
  font-size: 12px;
}

.hero-sms-sync span {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #27ce94;
  box-shadow: 0 0 0 4px rgba(39, 206, 148, 0.12);
}

.hero-sms-sync.is-refreshing span { animation: hero-sms-pulse 1s infinite; }

@keyframes hero-sms-pulse {
  50% { opacity: 0.35; transform: scale(0.7); }
}

.hero-sms-workspace {
  width: min(1480px, 100%);
  margin: 0 auto;
  padding: 40px 24px 72px;
}

.hero-sms-intro,
.hero-sms-panel-heading,
.hero-sms-task-heading {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 22px;
}

.hero-sms-intro { margin-bottom: 26px; }

.hero-sms-kicker,
.hero-sms-panel-heading > div > span,
.hero-sms-task-heading > div > span {
  margin: 0 0 7px;
  color: #2465df;
  font-size: 10px;
  font-weight: 900;
  letter-spacing: 0.16em;
}

.hero-sms-intro h1 {
  margin: 0;
  font-size: clamp(30px, 4vw, 45px);
  line-height: 1.15;
  letter-spacing: -0.045em;
}

.hero-sms-intro p:last-child {
  max-width: 760px;
  margin: 11px 0 0;
  color: #6f7e93;
  font-size: 14px;
  line-height: 1.7;
}

.hero-sms-intro > a {
  flex: 0 0 auto;
  padding: 9px 13px;
  color: #36516f;
  border: 1px solid #d5deea;
  border-radius: 10px;
  background: rgba(255, 255, 255, 0.72);
  font-size: 12px;
  font-weight: 700;
  text-decoration: none;
}

.hero-sms-error { margin-bottom: 18px; }

.hero-sms-layout {
  display: grid;
  grid-template-columns: minmax(310px, 390px) minmax(0, 1fr);
  gap: 22px;
  align-items: start;
}

.hero-sms-purchase-panel {
  position: sticky;
  top: 90px;
  padding: 22px;
  border: 1px solid #dfe6f0;
  border-radius: 20px;
  background: rgba(255, 255, 255, 0.92);
  box-shadow: 0 14px 38px rgba(30, 47, 76, 0.08);
}

.hero-sms-panel-heading,
.hero-sms-task-heading { margin-bottom: 20px; }

.hero-sms-panel-heading h2,
.hero-sms-task-heading h2 {
  margin: 0;
  font-size: 20px;
  letter-spacing: -0.02em;
}

.hero-sms-purchase-panel :deep(.el-select),
.hero-sms-purchase-panel :deep(.el-input-number) { width: 100%; }

.hero-sms-purchase-panel :deep(.el-form-item__label) {
  color: #44536a;
  font-size: 12px;
  font-weight: 800;
}

.hero-sms-purchase-panel :deep(.el-radio-group) { width: 100%; }
.hero-sms-purchase-panel :deep(.el-radio-button) { flex: 1; }
.hero-sms-purchase-panel :deep(.el-radio-button__inner) { width: 100%; }

.hero-sms-field-empty {
  width: 100%;
  padding: 9px 12px;
  color: #8a97a9;
  border: 1px dashed #d7dfea;
  border-radius: 8px;
  background: #fafbfd;
  font-size: 12px;
}

.hero-sms-form-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(120px, 0.8fr);
  gap: 12px;
  align-items: start;
}

.hero-sms-price-summary {
  display: flex;
  min-height: 61px;
  margin-top: 21px;
  padding: 8px 11px;
  flex-direction: column;
  justify-content: center;
  gap: 3px;
  border: 1px solid #e0e6ef;
  border-radius: 8px;
  background: #f7f9fc;
}

.hero-sms-price-summary span { color: #7d8b9f; font-size: 10px; }
.hero-sms-price-summary strong { color: #2b3b52; font-size: 13px; }

.hero-sms-start {
  width: 100%;
  height: 43px;
  margin-top: 16px;
  font-weight: 800;
}

.hero-sms-form-note {
  margin-top: 18px;
  padding: 14px;
  border-radius: 12px;
  background: #f0f5fd;
}

.hero-sms-form-note strong { color: #28548c; font-size: 12px; }
.hero-sms-form-note p { margin: 5px 0 0; color: #6a7d96; font-size: 11px; line-height: 1.65; }

.hero-sms-task-area { min-width: 0; }

.hero-sms-stats {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 10px;
  margin-bottom: 15px;
}

.hero-sms-stats > div {
  display: flex;
  align-items: center;
  justify-content: space-between;
  min-height: 61px;
  padding: 11px 14px;
  border: 1px solid #dfe6f0;
  border-radius: 13px;
  background: rgba(255, 255, 255, 0.78);
}

.hero-sms-stats span { color: #79889d; font-size: 11px; }
.hero-sms-stats strong { color: #1d4f9f; font-size: 21px; }

.hero-sms-task-list { display: grid; gap: 16px; }
.hero-sms-empty-copy { margin: -20px 0 0; color: #8491a4; font-size: 12px; }

@media (max-width: 1040px) {
  .hero-sms-layout { grid-template-columns: 1fr; }
  .hero-sms-purchase-panel { position: static; }
}

@media (max-width: 680px) {
  .hero-sms-topbar { padding: 0 16px; }
  .hero-sms-sync { font-size: 0; }
  .hero-sms-workspace { padding: 28px 14px 56px; }
  .hero-sms-intro { align-items: flex-start; flex-direction: column; }
  .hero-sms-stats { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
</style>
