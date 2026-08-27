export type UnknownRecord = Record<string, unknown>

export interface SMSBowerSettings {
  apiKey: string
  configured?: boolean
}

export interface CatalogOption {
  key: string
  value: string
  label: string
  description?: string
  raw: UnknownRecord
}

export type PriceTier = 'Bronze' | 'Silver' | 'Gold'

export interface PriceOption extends CatalogOption {
  price?: number
  provider?: string
  stock?: number
  tier?: PriceTier
  tierDerived?: boolean
}

export interface BatchRequest {
  service: string
  service_name?: string
  country: string
  country_name?: string
  price?: number
  max_price?: string
  provider?: string
  provider_ids?: number[]
  quantity: number
  pin: string
  proxy?: string
  proxy_pool?: string
}

export interface BatchView {
  id: string
  status: string
  total: number
  purchased: number
  completed: number
  successful: number
  inflight: number
  proxyAvailable: number
  proxyTotal: number
  failed: number
  message: string
  createdAt?: string
}

export interface VerificationCodeView {
  id: string
  code: string
  receivedAt?: string
}

export interface ActivationView {
  id: string
  activationId?: string
  batchId?: string
  phone: string
  service?: string
  country?: string
  provider?: string
  status: string
  balance?: number
  loginCode?: string
  pinCode?: string
  subsequentCodes: VerificationCodeView[]
  error?: string
  createdAt?: string
  expiresAt?: string
  finishedAt?: string
  raw: UnknownRecord
}

export type GoPayLoginState = 'valid' | 'invalid' | 'unknown' | 'checking'

export interface GoPayLoginStatusView {
  id: string
  phone: string
  status: GoPayLoginState
  valid?: boolean
  checkedAt?: string
  message?: string
  refreshed: boolean
  raw: UnknownRecord
}

export interface StatusMeta {
  label: string
  type: 'primary' | 'success' | 'warning' | 'danger' | 'info'
  active: boolean
}
