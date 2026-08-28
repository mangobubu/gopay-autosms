import type { HeroSmsNumberTask } from './heroSmsTypes'

export interface HeroSmsCountdownPresentation {
  expired: boolean
  remainingMs?: number
  label: string
}

export interface HeroSmsRefundPresentation extends HeroSmsCountdownPresentation {
  eligible: boolean
  reason: 'waiting_purchase' | 'eligible' | 'message_received' | 'window_elapsed' | 'not_refundable' | 'unknown'
}

function timestamp(value?: string): number | undefined {
  if (!value) return undefined
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) ? parsed : undefined
}

export function formatHeroSmsDuration(durationMs: number): string {
  if (!Number.isFinite(durationMs)) return '—'
  const totalSeconds = Math.max(0, Math.ceil(durationMs / 1_000))
  const days = Math.floor(totalSeconds / 86_400)
  const hours = Math.floor((totalSeconds % 86_400) / 3_600)
  const minutes = Math.floor((totalSeconds % 3_600) / 60)
  const seconds = totalSeconds % 60
  const clock = [hours, minutes, seconds].map((part) => String(part).padStart(2, '0')).join(':')
  return days > 0 ? `${days}天 ${clock}` : clock
}

export function heroSmsCountdown(
  endsAt?: string,
  nowMs = Date.now(),
): HeroSmsCountdownPresentation {
  const endMs = timestamp(endsAt)
  if (endMs === undefined) return { expired: false, label: '—' }
  const remainingMs = Math.max(0, endMs - nowMs)
  return {
    expired: remainingMs === 0,
    remainingMs,
    label: formatHeroSmsDuration(remainingMs),
  }
}

export function heroSmsRefundCountdown(
  task: Pick<HeroSmsNumberTask, 'phone' | 'messages' | 'refundable' | 'refundStatus' | 'refundableUntil'>,
  nowMs = Date.now(),
): HeroSmsRefundPresentation {
  if (!task.phone) {
    return {
      eligible: false,
      expired: false,
      label: '号码购买后开始',
      reason: 'waiting_purchase',
    }
  }
  if (task.refundStatus === 'requested') {
    return {
      eligible: false,
      expired: false,
      label: '退款处理中',
      reason: 'not_refundable',
    }
  }
  if (task.refundStatus === 'refunded') {
    return {
      eligible: false,
      expired: false,
      label: '已退款',
      reason: 'not_refundable',
    }
  }
  if (task.refundStatus === 'settled') {
    return {
      eligible: false,
      expired: false,
      label: '已结算，不可退款',
      reason: 'not_refundable',
    }
  }
  if (task.messages.length > 0 || task.refundStatus === 'forfeited_by_message') {
    return {
      eligible: false,
      expired: false,
      label: '已收到验证码，不可退款',
      reason: 'message_received',
    }
  }
  const explicitlyIneligible = task.refundable === false
    || ['window_elapsed', 'not_applicable', 'unavailable'].includes(task.refundStatus ?? '')
  if (explicitlyIneligible) {
    return {
      eligible: false,
      expired: task.refundStatus === 'window_elapsed',
      label: '不可退款',
      reason: task.refundStatus === 'window_elapsed' ? 'window_elapsed' : 'not_refundable',
    }
  }

  const countdown = heroSmsCountdown(task.refundableUntil, nowMs)
  if (countdown.remainingMs !== undefined) {
    if (countdown.expired) {
      return {
        ...countdown,
        eligible: false,
        label: '已超过退款时限',
        reason: 'window_elapsed',
      }
    }
    return {
      ...countdown,
      eligible: true,
      reason: 'eligible',
    }
  }
  if (task.refundable === true || ['eligible', 'refundable'].includes(task.refundStatus ?? '')) {
    return {
      eligible: true,
      expired: false,
      label: '可退款（以服务端为准）',
      reason: 'eligible',
    }
  }
  return {
    eligible: false,
    expired: false,
    label: '退款状态待确认',
    reason: 'unknown',
  }
}
