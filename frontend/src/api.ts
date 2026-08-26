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
    request<unknown>(withQuery('/api/catalog/prices', { service, country })),

  createBatch: (batch: BatchRequest) =>
    request<unknown>('/api/batches', {
      method: 'POST',
      body: JSON.stringify(batch),
    }),

  getBatch: (id: string) => request<unknown>(`/api/batches/${encodeURIComponent(id)}`),

  getActivations: (batchId?: string) =>
    request<unknown>(withQuery('/api/activations', { batch_id: batchId })),

  markSuccess: (id: string) =>
    request<unknown>(`/api/activations/${encodeURIComponent(id)}/success`, {
      method: 'POST',
    }),

  deleteActivation: (id: string) =>
    request<void>(`/api/activations/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    }),
}
