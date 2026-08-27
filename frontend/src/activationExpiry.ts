import { formatCountdown, formatTime } from './normalizers.ts'

export interface ActivationExpiryPresentation {
  label: string
  countdown?: string
}

const PROVIDER_CANCELLED_CLASSIFICATIONS = new Set([
  'duplicate',
  'login_code_timeout',
  'pin_code_timeout',
  'pin_required',
  'unregistered',
])

const PROVIDER_SETTLED_CLASSIFICATIONS = new Set([
  'pin_submission_blocked',
])

function isCancelledActivation(status: string, finishedAt?: string): boolean {
  return status === 'cancelled'
    || (Boolean(finishedAt) && PROVIDER_CANCELLED_CLASSIFICATIONS.has(status))
}

export function activationExpiryPresentation(
  status: string,
  expiresAt?: string,
  finishedAt?: string,
  now = Date.now(),
): ActivationExpiryPresentation {
  if (isCancelledActivation(status, finishedAt)) {
    return { label: '已取消' }
  }
  if (finishedAt && PROVIDER_SETTLED_CLASSIFICATIONS.has(status)) {
    return { label: '已结算' }
  }

  const countdown = formatCountdown(expiresAt, now)
  return {
    label: formatTime(expiresAt),
    countdown: countdown === '—' ? undefined : countdown,
  }
}
