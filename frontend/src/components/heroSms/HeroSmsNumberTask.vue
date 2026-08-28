<script setup lang="ts">
import { computed } from 'vue'
import { ElMessage } from 'element-plus'

import { heroSmsCountdown, heroSmsRefundCountdown } from '../../heroSmsCountdown'
import { heroSmsTaskStatus, verificationTypeLabel } from '../../heroSmsNormalizers'
import type { HeroSmsNumberTask, HeroSmsTaskAction } from '../../heroSmsTypes'

const props = defineProps<{
  task: HeroSmsNumberTask
  nowMs: number
  busy?: boolean
}>()

const emit = defineEmits<{
  action: [task: HeroSmsNumberTask, action: HeroSmsTaskAction]
}>()

const validity = computed(() => heroSmsCountdown(props.task.expiresAt, props.nowMs))
const statusMeta = computed(() => {
  const meta = heroSmsTaskStatus(props.task.status)
  if (props.task.running || !meta.active) return meta
  return validity.value.expired
    ? { label: '号码已过期', type: 'info' as const, active: false }
    : { ...meta, active: false }
})
const refund = computed(() => heroSmsRefundCountdown(props.task, props.nowMs))
const waitingForStock = computed(() => ['waiting_number', 'waiting_inventory', 'waiting_stock'].includes(props.task.status))
const canCancel = computed(() => props.task.capabilities.cancel && refund.value.eligible)
const hasAction = computed(() => props.task.capabilities.start
  || props.task.capabilities.stop
  || props.task.capabilities.settle
  || canCancel.value)
const durationLabel = computed(() => {
  const seconds = props.task.effectiveDurationSeconds ?? props.task.requestedDurationSeconds
  if (!seconds) return props.task.productKind === 'rent' ? '租期确认中' : '标准有效期'
  if (seconds % 86_400 === 0) return `${seconds / 86_400} 天`
  if (seconds % 3_600 === 0) return `${seconds / 3_600} 小时`
  if (seconds % 60 === 0) return `${seconds / 60} 分钟`
  return `${seconds} 秒`
})

function formatDateTime(value?: string): string {
  if (!value) return '—'
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '—'
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date)
}

function priceLabel(): string {
  if (props.task.price === undefined) return '购买后确认'
  const amount = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 4 }).format(props.task.price)
  return [amount, props.task.currency].filter(Boolean).join(' ')
}

async function copy(value: string | undefined, successMessage: string): Promise<void> {
  if (!value) return
  try {
    await navigator.clipboard.writeText(value)
    ElMessage.success(successMessage)
  } catch {
    ElMessage.warning('复制失败，请手动选择内容')
  }
}
</script>

<template>
  <article class="hero-task" :class="{ 'hero-task--active': statusMeta.active }">
    <header class="hero-task__header">
      <div class="hero-task__identity">
        <span class="hero-task__index">独立号码任务</span>
        <button
          v-if="task.phone"
          class="hero-task__phone"
          type="button"
          title="复制号码"
          @click="copy(task.phone, '号码已复制')"
        >
          {{ task.phone }}
          <span aria-hidden="true">⧉</span>
        </button>
        <strong v-else class="hero-task__pending-phone">
          {{ task.running ? (waitingForStock ? '等待可用号码' : '正在分配号码') : '未购买号码' }}
        </strong>
      </div>

      <el-tag :type="statusMeta.type" effect="light" round>
        <span v-if="statusMeta.active" class="hero-task__pulse" aria-hidden="true" />
        {{ statusMeta.label }}
      </el-tag>
    </header>

    <el-alert
      v-if="waitingForStock"
      class="hero-task__notice"
      title="当前暂无可用号码，任务会保持运行并持续尝试购买。"
      type="warning"
      :closable="false"
      show-icon
    />

    <div class="hero-task__facts">
      <div>
        <span>服务 / 国家</span>
        <strong>{{ task.serviceName || task.service || '—' }} · {{ task.countryName || task.country || '—' }}</strong>
      </div>
      <div>
        <span>验证方式</span>
        <strong>{{ task.verificationType ? verificationTypeLabel(task.verificationType) : '由服务决定' }}</strong>
      </div>
      <div>
        <span>时长</span>
        <strong>{{ durationLabel }}</strong>
      </div>
      <div>
        <span>购买价格</span>
        <strong>{{ priceLabel() }}</strong>
      </div>
    </div>

    <div class="hero-task__countdowns">
      <section>
        <div class="hero-task__countdown-label">
          <span>号码有效期</span>
          <small>{{ task.expiresAt ? `至 ${formatDateTime(task.expiresAt)}` : '购买后开始' }}</small>
        </div>
        <strong :class="{ 'is-expired': validity.expired }">
          {{ task.expiresAt ? (validity.expired ? '已到期' : validity.label) : '等待号码' }}
        </strong>
      </section>
      <section :class="{ 'is-refundable': refund.eligible }">
        <div class="hero-task__countdown-label">
          <span>退款时限</span>
          <small>{{ task.refundableUntil ? `至 ${formatDateTime(task.refundableUntil)}` : '以后端状态为准' }}</small>
        </div>
        <strong :class="{ 'is-expired': refund.expired }">{{ refund.label }}</strong>
      </section>
    </div>

    <section class="hero-task__messages">
      <div class="hero-task__messages-head">
        <div>
          <span>验证码</span>
          <el-tag v-if="task.messages.length" type="success" size="small" round>
            {{ task.messages.length }} 条
          </el-tag>
        </div>
        <small v-if="task.running">Webhook 优先接收 · 自动同步</small>
      </div>

      <div v-if="task.messages.length" class="hero-task__message-list">
        <article v-for="(message, index) in task.messages" :key="message.id" class="hero-task__message">
          <div>
            <span>第 {{ index + 1 }} 条</span>
            <time>{{ formatDateTime(message.receivedAt) }}</time>
          </div>
          <button
            v-if="message.code"
            type="button"
            title="复制验证码"
            @click="copy(message.code, '验证码已复制')"
          >
            {{ message.code }}
            <span aria-hidden="true">⧉</span>
          </button>
          <p v-if="message.text">{{ message.text }}</p>
        </article>
      </div>
      <div v-else class="hero-task__message-empty">
        <span v-if="task.running" class="hero-task__waiting-dot" aria-hidden="true" />
        {{ task.running
          ? (task.phone ? '号码保持运行，正在等待验证码…' : '购买号码后开始接收验证码')
          : (task.phone ? '任务已结束，未收到验证码' : '任务已停止，未购买号码') }}
      </div>
    </section>

    <el-alert
      v-if="task.error"
      class="hero-task__notice"
      :title="task.error"
      type="error"
      :closable="false"
      show-icon
    />

    <footer class="hero-task__footer">
      <div class="hero-task__reference">
        <span>任务 ID</span>
        <code>{{ task.id }}</code>
        <small v-if="task.retryCount">已尝试 {{ task.retryCount }} 次</small>
      </div>

      <div v-if="hasAction" class="hero-task__actions">
        <el-button
          v-if="task.capabilities.start"
          type="primary"
          :loading="busy"
          @click="emit('action', task, 'start')"
        >
          继续购买
        </el-button>
        <el-button
          v-if="task.capabilities.stop"
          :loading="busy"
          @click="emit('action', task, 'stop')"
        >
          停止购买
        </el-button>
        <el-button
          v-if="canCancel"
          type="success"
          plain
          :loading="busy"
          @click="emit('action', task, 'cancel')"
        >
          申请退款
        </el-button>
        <el-button
          v-if="task.capabilities.settle"
          type="danger"
          plain
          :loading="busy"
          @click="emit('action', task, 'settle')"
        >
          停止并结算
        </el-button>
      </div>
    </footer>
  </article>
</template>

<style scoped>
.hero-task {
  overflow: hidden;
  border: 1px solid #dfe6f0;
  border-radius: 20px;
  background: #fff;
  box-shadow: 0 12px 32px rgba(28, 45, 75, 0.07);
}

.hero-task--active {
  border-color: #cddbf4;
  box-shadow: 0 14px 36px rgba(36, 101, 229, 0.1);
}

.hero-task__header,
.hero-task__footer,
.hero-task__messages-head,
.hero-task__countdown-label,
.hero-task__message > div {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}

.hero-task__header {
  padding: 20px 22px 17px;
  border-bottom: 1px solid #edf1f6;
}

.hero-task__identity {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 5px;
}

.hero-task__index,
.hero-task__facts span,
.hero-task__countdown-label span,
.hero-task__messages-head > div > span,
.hero-task__reference > span {
  color: #78859a;
  font-size: 11px;
  font-weight: 800;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}

.hero-task__phone {
  width: fit-content;
  padding: 0;
  color: #172235;
  border: 0;
  background: none;
  font-size: clamp(22px, 3vw, 29px);
  font-weight: 800;
  letter-spacing: 0.015em;
  cursor: pointer;
}

.hero-task__phone span,
.hero-task__message button span {
  margin-left: 5px;
  color: #8b98ac;
  font-size: 13px;
  font-weight: 500;
}

.hero-task__pending-phone {
  color: #41516a;
  font-size: 20px;
}

.hero-task__pulse,
.hero-task__waiting-dot {
  display: inline-block;
  width: 7px;
  height: 7px;
  margin-right: 6px;
  border-radius: 50%;
  background: #26bd84;
  box-shadow: 0 0 0 4px rgba(38, 189, 132, 0.12);
  animation: hero-task-pulse 1.8s infinite;
}

@keyframes hero-task-pulse {
  50% { opacity: 0.5; transform: scale(0.75); }
}

.hero-task__notice {
  margin: 16px 22px 0;
}

.hero-task__facts {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 1px;
  margin: 18px 22px;
  overflow: hidden;
  border: 1px solid #e5eaf2;
  border-radius: 14px;
  background: #e5eaf2;
}

.hero-task__facts > div {
  display: flex;
  min-width: 0;
  min-height: 74px;
  padding: 14px;
  flex-direction: column;
  justify-content: center;
  gap: 6px;
  background: #f8fafc;
}

.hero-task__facts strong {
  overflow: hidden;
  color: #26344a;
  font-size: 13px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.hero-task__countdowns {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 14px;
  padding: 0 22px 18px;
}

.hero-task__countdowns section {
  padding: 15px 16px;
  border: 1px solid #dce6f4;
  border-radius: 14px;
  background: linear-gradient(135deg, #f5f8fd, #fbfcfe);
}

.hero-task__countdowns section.is-refundable {
  border-color: #bde8d7;
  background: linear-gradient(135deg, #effbf6, #fbfefd);
}

.hero-task__countdown-label {
  align-items: flex-start;
}

.hero-task__countdown-label small {
  color: #8996a8;
  font-size: 10px;
}

.hero-task__countdowns section > strong {
  display: block;
  margin-top: 10px;
  color: #1d5fd1;
  font-size: 20px;
  font-variant-numeric: tabular-nums;
}

.hero-task__countdowns section.is-refundable > strong {
  color: #0d9668;
}

.hero-task__countdowns strong.is-expired {
  color: #d05252;
}

.hero-task__messages {
  margin: 0 22px 18px;
  padding: 17px;
  border: 1px solid #e3e8f0;
  border-radius: 16px;
  background: #fbfcfe;
}

.hero-task__messages-head > div {
  display: flex;
  align-items: center;
  gap: 8px;
}

.hero-task__messages-head > small {
  color: #76859a;
}

.hero-task__message-list {
  display: grid;
  gap: 9px;
  margin-top: 14px;
}

.hero-task__message {
  padding: 12px 14px;
  border: 1px solid #dfe7f0;
  border-radius: 12px;
  background: #fff;
}

.hero-task__message > div span,
.hero-task__message time {
  color: #8190a4;
  font-size: 11px;
}

.hero-task__message button {
  margin-top: 7px;
  padding: 0;
  color: #185bd1;
  border: 0;
  background: none;
  font-size: 25px;
  font-weight: 800;
  letter-spacing: 0.12em;
  cursor: pointer;
}

.hero-task__message p {
  margin: 8px 0 0;
  color: #5e6e84;
  font-size: 12px;
  line-height: 1.6;
  word-break: break-word;
}

.hero-task__message-empty {
  display: flex;
  align-items: center;
  min-height: 52px;
  margin-top: 12px;
  color: #748298;
  font-size: 13px;
}

.hero-task__footer {
  align-items: flex-end;
  padding: 16px 22px;
  border-top: 1px solid #edf1f6;
  background: #fbfcfe;
}

.hero-task__reference {
  display: grid;
  min-width: 0;
  gap: 3px;
}

.hero-task__reference code {
  overflow: hidden;
  max-width: 340px;
  color: #4f6077;
  font-size: 11px;
  text-overflow: ellipsis;
}

.hero-task__reference small {
  color: #8a97aa;
}

.hero-task__actions {
  display: flex;
  justify-content: flex-end;
  flex-wrap: wrap;
  gap: 8px;
}

.hero-task__actions :deep(.el-button + .el-button) {
  margin-left: 0;
}

@media (max-width: 820px) {
  .hero-task__facts { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}

@media (max-width: 620px) {
  .hero-task__header,
  .hero-task__footer {
    align-items: flex-start;
    flex-direction: column;
  }

  .hero-task__facts,
  .hero-task__countdowns { grid-template-columns: 1fr; }
  .hero-task__actions { width: 100%; justify-content: flex-start; }
  .hero-task__messages-head { align-items: flex-start; flex-direction: column; }
}
</style>
