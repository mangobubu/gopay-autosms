import type { BatchRequest, SMSBowerSettings } from './types'

export class ApiError extends Error {
  readonly status: number
  readonly payload: unknown

  constructor(message: string, status: number, payload: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

function errorMessage(payload: unknown, fallback: string): string {
  if (typeof payload === 'string' && payload.trim()) return payload
  if (!payload || typeof payload !== 'object') return fallback

  const record = payload as Record<string, unknown>
  for (const key of ['message', 'error', 'detail']) {
    const value = record[key]
    if (typeof value === 'string' && value.trim()) return value
  }
  return fallback
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.body !== undefined) headers.set('Content-Type', 'application/json')

  let response: Response
  try {
    response = await fetch(path, {
      ...init,
      headers,
      credentials: 'same-origin',
    })
  } catch (error) {
    const detail = error instanceof Error ? error.message : '网络连接失败'
    throw new ApiError(`服务连接失败：${detail}`, 0, null)
  }

  const contentType = response.headers.get('content-type') ?? ''
  let payload: unknown = null
  if (response.status !== 204) {
    payload = contentType.includes('application/json')
      ? await response.json().catch(() => null)
      : await response.text().catch(() => '')
  }

  if (!response.ok) {
    throw new ApiError(
      errorMessage(payload, `请求失败（HTTP ${response.status}）`),
      response.status,
      payload,
    )
  }

  return payload as T
}

function withQuery(path: string, params: Record<string, string | number | undefined>): string {
  const query = new URLSearchParams()
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== '') query.set(key, String(value))
  })
  const suffix = query.toString()
  return suffix ? `${path}?${suffix}` : path
}

export function validateAccountLoginStatusesPayload(payload: unknown): unknown {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) {
    throw new ApiError('GoPay 登录状态响应格式异常', 502, payload)
  }
  const accounts = (payload as Record<string, unknown>).accounts
  if (!Array.isArray(accounts)) {
    throw new ApiError('GoPay 登录状态响应格式异常', 502, payload)
  }

  const allowedStates = new Set(['valid', 'invalid', 'unknown', 'checking'])
  const seenIDs = new Set<string>()
  const statusValue = (value: unknown): string | undefined => {
    if (typeof value !== 'string') return undefined
    const normalized = value.trim().toLowerCase().replace(/[\s-]+/g, '_')
    return normalized || undefined
  }
  const accountID = (value: unknown): string | undefined => {
    if (typeof value === 'number') {
      if (!Number.isSafeInteger(value) || value <= 0) return undefined
      return String(value)
    }
    if (typeof value !== 'string') return undefined
    const normalized = value.trim()
    if (!/^[1-9]\d*$/.test(normalized)) return undefined
    const parsed = Number(normalized)
    if (!Number.isSafeInteger(parsed) || parsed <= 0) return undefined
    return String(parsed)
  }
  const validAccount = accounts.every((account) => {
    if (!account || typeof account !== 'object' || Array.isArray(account)) return false
    const record = account as Record<string, unknown>
    const id = accountID(record.id ?? record.account_id ?? record.accountId)
    if (!id || seenIDs.has(id)) return false
    seenIDs.add(id)

    const phone = record.phone_number ?? record.phoneNumber ?? record.phone
    if (typeof phone !== 'string' || phone.trim() === '') return false

    const loginStatus = statusValue(record.login_status ?? record.loginStatus)
    const stateAlias = statusValue(record.status ?? record.state)
    if ((!loginStatus && !stateAlias)
      || (loginStatus && !allowedStates.has(loginStatus))
      || (stateAlias && !allowedStates.has(stateAlias))) return false
    if (loginStatus && stateAlias && loginStatus !== stateAlias) return false

    if ('valid' in record
      && typeof record.valid !== 'boolean') return false
    if ('is_valid' in record
      && typeof record.is_valid !== 'boolean') return false
    if ('isValid' in record
      && typeof record.isValid !== 'boolean') return false
    const valid = record.valid ?? record.is_valid ?? record.isValid
    const effectiveStatus = loginStatus ?? stateAlias
    if (typeof valid === 'boolean'
      && ((effectiveStatus === 'valid') !== valid)
      && effectiveStatus !== 'checking') return false

    if ('checked_at' in record && (typeof record.checked_at !== 'string'
      || !Number.isFinite(Date.parse(record.checked_at)))) return false
    if ('checkedAt' in record && (typeof record.checkedAt !== 'string'
      || !Number.isFinite(Date.parse(record.checkedAt)))) return false
    const messageFields = ['error', 'message', 'detail']
    if (messageFields.some((field) => field in record
      && record[field] !== undefined
      && typeof record[field] !== 'string')) return false
    if ('refreshed' in record && typeof record.refreshed !== 'boolean') return false
    if ('was_refreshed' in record && typeof record.was_refreshed !== 'boolean') return false
    if ('wasRefreshed' in record && typeof record.wasRefreshed !== 'boolean') return false
    return true
  })
  if (!validAccount) {
    throw new ApiError('GoPay 登录状态响应格式异常', 502, payload)
  }
  return payload
}

export const api = {
  getSettings: () => request<unknown>('/api/settings/smsbower'),

  saveSettings: (settings: SMSBowerSettings) =>
    request<unknown>('/api/settings/smsbower', {
      method: 'PUT',
      body: JSON.stringify({
        api_key: settings.apiKey,
      }),
    }),

  getServices: () => request<unknown>('/api/catalog/services'),

  getCountries: (service: string) =>
    request<unknown>(withQuery('/api/catalog/countries', { service })),

  getPrices: (service: string, country: string) =>
    request<unknown>(withQuery('/api/catalog/prices', { service, country }), {
      cache: 'no-store',
    }),

  createBatch: (batch: BatchRequest) =>
    request<unknown>('/api/batches', {
      method: 'POST',
      body: JSON.stringify(batch),
    }),

  getBatches: () => request<unknown>('/api/batches'),

  getBatch: (id: string) => request<unknown>(`/api/batches/${encodeURIComponent(id)}`),

  stopBatch: (id: string) =>
    request<unknown>(`/api/batches/${encodeURIComponent(id)}/stop`, {
      method: 'POST',
    }),

  getActivations: (batchId?: string) =>
    request<unknown>(withQuery('/api/activations', { batch_id: batchId })),

  getAccountLoginStatuses: async (signal?: AbortSignal) =>
    validateAccountLoginStatusesPayload(await request<unknown>('/api/accounts/login-status', {
      cache: 'no-store',
      signal,
    })),

  refreshAccountLoginStatuses: async (signal?: AbortSignal) =>
    validateAccountLoginStatusesPayload(await request<unknown>('/api/accounts/login-status/refresh', {
      method: 'POST',
      cache: 'no-store',
      signal,
    })),

  markSuccess: (id: string) =>
    request<unknown>(`/api/activations/${encodeURIComponent(id)}/success`, {
      method: 'POST',
    }),

  deleteActivation: (id: string) =>
    request<void>(`/api/activations/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    }),
}
