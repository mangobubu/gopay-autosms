<script setup lang="ts">
import { computed } from 'vue'
import { ElMessage } from 'element-plus'

import { activationExpiryPresentation } from '../activationExpiry'
import {
  accountLoginStatus,
  activationStatus,
  formatNumber,
  formatTime,
} from '../normalizers'
import type { ActivationView, GoPayLoginStatusView } from '../types'

const props = defineProps<{
  activation: ActivationView
  loginStatus?: GoPayLoginStatusView
  busy?: boolean
  nowMs?: number
}>()

const emit = defineEmits<{
  success: [activation: ActivationView]
  delete: [activation: ActivationView]
}>()

const meta = computed(() => activationStatus(props.activation.status))
const loginMeta = computed(() => (
  props.loginStatus ? accountLoginStatus(props.loginStatus.status) : undefined
))
const canOperate = computed(() => !props.activation.finishedAt && ['polling', 'active', 'awaiting_subsequent_code'].includes(props.activation.status))
const isPolling = computed(() => !props.activation.finishedAt && ['polling', 'active', 'awaiting_subsequent_code', 'pin_changed'].includes(props.activation.status))
const expiry = computed(() => activationExpiryPresentation(
  props.activation.status,
  props.activation.expiresAt,
  props.activation.finishedAt,
  props.nowMs,
))

async function copyPhone(): Promise<void> {
  if (!props.activation.phone || props.activation.phone === '—') return
  try {
    await navigator.clipboard.writeText(props.activation.phone)
    ElMessage.success('号码已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择号码')
  }
}
</script>

<template>
  <article class="activation-card" :class="{ 'is-polling': isPolling }">
    <header class="activation-card__header">
      <div class="activation-card__identity">
        <span class="activation-card__eyebrow">手机号码</span>
        <button class="phone-number" type="button" title="复制号码" @click="copyPhone">
          {{ activation.phone }}
          <span class="copy-mark" aria-hidden="true">⧉</span>
        </button>
      </div>

      <div class="activation-card__status-stack">
        <el-tag :type="meta.type" effect="light" round>
          <span v-if="meta.active" class="status-pulse" aria-hidden="true" />
          {{ meta.label }}
        </el-tag>

        <div
          v-if="loginStatus && loginMeta"
          class="activation-login-status"
          :class="`is-${loginStatus.status}`"
          :title="loginStatus.message"
          role="status"
          aria-live="polite"
        >
          <div class="activation-login-status__primary">
            <span class="activation-login-status__dot" aria-hidden="true" />
            <span>GoPay · {{ loginMeta.label }}</span>
          </div>
          <small v-if="loginStatus.checkedAt">
            最近检查 {{ formatTime(loginStatus.checkedAt) }}
          </small>
          <small v-if="loginStatus.refreshed">已自动刷新登录凭据</small>
        </div>
      </div>
    </header>

    <div class="activation-card__meta">
      <div>
        <span>余额</span>
        <strong :class="{ 'balance-positive': (activation.balance ?? 0) >= 1 }">
          {{ activation.balance === undefined ? '查询中' : `${formatNumber(activation.balance)} RP` }}
        </strong>
      </div>
      <div>
        <span>供应商</span>
        <strong>{{ activation.provider || '—' }}</strong>
      </div>
      <div>
        <span>到期时间</span>
        <strong>{{ expiry.label }}</strong>
        <small v-if="expiry.countdown" class="activation-countdown">倒计时 {{ expiry.countdown }}</small>
      </div>
    </div>

    <div class="code-grid">
      <section class="code-panel code-panel--login">
        <span class="code-panel__label">登录验证码</span>
        <strong>{{ activation.loginCode || '等待接收' }}</strong>
      </section>
      <section class="code-panel code-panel--pin">
        <span class="code-panel__label">改 PIN 验证码</span>
        <strong>{{ activation.pinCode || '等待接收' }}</strong>
      </section>
    </div>

    <section class="subsequent-codes">
      <div class="subsequent-codes__title">
        <span>后续验证码</span>
        <small v-if="isPolling">每 2 秒自动轮询</small>
      </div>

      <div v-if="activation.subsequentCodes.length" class="code-chips">
        <div
          v-for="(item, index) in activation.subsequentCodes"
          :key="item.id"
          class="code-chip"
        >
          <span>{{ index + 1 }}.</span>
          <strong>{{ item.code }}</strong>
          <time v-if="item.receivedAt">{{ formatTime(item.receivedAt) }}</time>
        </div>
      </div>
      <div v-else class="code-empty">
        {{ isPolling ? '正在等待新的验证码…' : '暂无后续验证码' }}
      </div>
    </section>

    <el-alert
      v-if="activation.error"
      class="activation-error"
      :title="activation.error"
      type="error"
      :closable="false"
      show-icon
    />

    <footer class="activation-card__footer">
      <div class="activation-reference">
        <span>ID</span>
        <code>{{ activation.activationId || activation.id }}</code>
      </div>
      <div v-if="canOperate" class="activation-actions">
        <el-button
          type="success"
          :loading="busy"
          @click="emit('success', activation)"
        >
          成功
        </el-button>
        <el-button
          type="danger"
          plain
          :loading="busy"
          @click="emit('delete', activation)"
        >
          删除
        </el-button>
      </div>
    </footer>
  </article>
</template>
