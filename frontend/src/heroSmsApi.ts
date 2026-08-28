import type {
  CreateHeroSmsTasksRequest,
  HeroSmsCatalogFilters,
  HeroSmsTaskAction,
} from './heroSmsTypes'

export class HeroSmsApiError extends Error {
  readonly status: number
  readonly payload: unknown

  constructor(message: string, status: number, payload: unknown) {
    super(message)
    this.name = 'HeroSmsApiError'
    this.status = status
    this.payload = payload
  }
}

function errorMessage(payload: unknown, fallback: string): string {
  if (typeof payload === 'string' && payload.trim()) return payload
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return fallback
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
    response = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  } catch (error) {
    const detail = error instanceof Error ? error.message : '网络连接失败'
    throw new HeroSmsApiError(`服务连接失败：${detail}`, 0, null)
  }
  const contentType = response.headers.get('content-type') ?? ''
  const payload = response.status === 204
    ? null
    : contentType.includes('application/json')
      ? await response.json().catch(() => null)
      : await response.text().catch(() => '')
  if (!response.ok) {
    throw new HeroSmsApiError(
      errorMessage(payload, `请求失败（HTTP ${response.status}）`),
      response.status,
      payload,
    )
  }
  return payload as T
}

function withQuery(path: string, values: Record<string, string | number | undefined>): string {
  const query = new URLSearchParams()
  Object.entries(values).forEach(([key, value]) => {
    if (value !== undefined && value !== '') query.set(key, String(value))
  })
  const suffix = query.toString()
  return suffix ? `${path}?${suffix}` : path
}

export function createHeroSmsIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID()
  return `hero-sms-${Date.now()}-${Math.random().toString(16).slice(2)}`
}

export const heroSmsApi = {
  getCatalog: (filters: HeroSmsCatalogFilters = {}) => request<unknown>(withQuery(
    '/api/hero-sms/catalog',
    {
      service: filters.service,
      country: filters.country,
      verification_type: filters.verificationType,
      duration_hours: filters.durationHours,
    },
  ), { cache: 'no-store' }),

  createTasks: (
    input: CreateHeroSmsTasksRequest,
    idempotencyKey = createHeroSmsIdempotencyKey(),
  ) => request<unknown>('/api/hero-sms/tasks', {
    method: 'POST',
    headers: { 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify({
      service: input.service,
      country: input.country,
      verification_type: input.verificationType,
      ...(input.durationHours !== undefined && input.durationHours > 0
        ? { duration_hours: input.durationHours }
        : {}),
      quantity: input.quantity,
    }),
  }),

  getTasks: (cursor?: string) => request<unknown>(withQuery('/api/hero-sms/tasks', {
    limit: 500,
    cursor,
  }), {
    cache: 'no-store',
  }),

  actOnTask: (id: string, action: HeroSmsTaskAction) => request<unknown>(
    `/api/hero-sms/tasks/${encodeURIComponent(id)}/${action}`,
    { method: 'POST' },
  ),
}
