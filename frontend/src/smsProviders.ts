import type { SMSProvider } from './types'

export interface SMSProviderProfile {
  value: SMSProvider
  displayName: string
  apiKeyPlaceholder: string
  apiKeyHint: string
  supportsPriceTiers: boolean
  priceCurrencyCode: string
  priceCurrencyLabel: string
}

const SMS_PROVIDER_PROFILES: Record<SMSProvider, SMSProviderProfile> = {
  smsbower: {
    value: 'smsbower',
    displayName: 'SMSBower',
    apiKeyPlaceholder: '输入 SMSBower API Key',
    apiKeyHint: '使用 SMSBower 账户中的 API Key',
    supportsPriceTiers: true,
    priceCurrencyCode: 'RUB',
    priceCurrencyLabel: '₽',
  },
  'hero-sms': {
    value: 'hero-sms',
    displayName: 'HeroSMS',
    apiKeyPlaceholder: '输入 HeroSMS API Key',
    apiKeyHint: '使用 HeroSMS 账户中的 API Key',
    supportsPriceTiers: false,
    // HeroSMS getPrices follows the account currency but does not include the
    // currency in its response. Do not guess a unit before getNumber returns it.
    priceCurrencyCode: '',
    priceCurrencyLabel: '',
  },
}

export const SMS_PROVIDERS = Object.freeze(
  Object.values(SMS_PROVIDER_PROFILES),
) as readonly SMSProviderProfile[]

export function smsProviderProfile(provider: SMSProvider): SMSProviderProfile {
  return SMS_PROVIDER_PROFILES[provider]
}

export function isSMSProvider(value: unknown): value is SMSProvider {
  return value === 'smsbower' || value === 'hero-sms'
}
