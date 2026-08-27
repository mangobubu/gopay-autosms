import { formatCountdown, formatTime } from './normalizers.ts'

export interface ActivationExpiryPresentation {
  label: string
  countdown?: string
}

const PROVIDER_CANCELLED_CLASSIFICATIONS = new Set([
  'duplicate',
  'login_code_timeout',
  'pin_required',
  'unregistered',
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

  const countdown = formatCountdown(expiresAt, now)
  return {
    label: formatTime(expiresAt),
    countdown: countdown === '—' ? undefined : countdown,
  }
}
