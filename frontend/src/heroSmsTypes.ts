export type HeroSmsRecord = Record<string, unknown>

export type HeroSmsProductKind = 'activation' | 'rent'
export type HeroSmsVerificationType = 'sms' | 'call' | string

export interface HeroSmsCatalogOption {
  key: string
  value: string
  label: string
  description?: string
  raw: HeroSmsRecord
}

export interface HeroSmsDurationOption {
  key: string
  seconds: number
  hours: number
  label: string
}

export interface HeroSmsOffer {
  key: string
  service: string
  serviceName?: string
  country: string
  countryName?: string
  verificationType?: HeroSmsVerificationType
  productKind: HeroSmsProductKind
  durationSeconds?: number
  durationHours?: number
  durationLabel?: string
  price?: number
  currency?: string
  stock?: number
  available: boolean
  refundableWindowSeconds?: number
  raw: HeroSmsRecord
}

export interface HeroSmsCatalog {
  services: HeroSmsCatalogOption[]
  countries: HeroSmsCatalogOption[]
  verificationTypes: HeroSmsCatalogOption[]
  durations: HeroSmsDurationOption[]
  offers: HeroSmsOffer[]
  message?: string
}

export interface HeroSmsMessage {
  id: string
  code?: string
  text?: string
  from?: string
  source?: 'webhook' | 'poll' | string
  receivedAt?: string
  raw: HeroSmsRecord
}

export interface HeroSmsTaskCapabilities {
  start: boolean
  stop: boolean
  settle: boolean
  cancel: boolean
}

export interface HeroSmsNumberTask {
  id: string
  status: string
  service: string
  serviceName?: string
  country: string
  countryName?: string
  verificationType?: HeroSmsVerificationType
  productKind: HeroSmsProductKind
  requestedDurationSeconds?: number
  effectiveDurationSeconds?: number
  providerActivationId?: string
  phone?: string
  operator?: string
  price?: number
  currency?: string
  purchasedAt?: string
  expiresAt?: string
  refundableUntil?: string
  refundStatus?: string
  refundable?: boolean
  nextAttemptAt?: string
  retryCount?: number
  error?: string
  running: boolean
  capabilities: HeroSmsTaskCapabilities
  messages: HeroSmsMessage[]
  createdAt?: string
  updatedAt?: string
  finishedAt?: string
  raw: HeroSmsRecord
}

export interface HeroSmsTaskEnvelope {
  tasks: HeroSmsNumberTask[]
  serverNow?: string
  nextCursor?: string
  message?: string
}

export interface HeroSmsCatalogFilters {
  service?: string
  country?: string
  verificationType?: HeroSmsVerificationType
  durationHours?: number
}

export interface CreateHeroSmsTasksRequest {
  service: string
  country: string
  verificationType: HeroSmsVerificationType
  durationHours?: number
  quantity: number
}

export type HeroSmsTaskAction = 'start' | 'stop' | 'settle' | 'cancel'
