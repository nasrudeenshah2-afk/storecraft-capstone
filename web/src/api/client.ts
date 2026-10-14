import type { components } from './schema'
import { ApiError, isErrorCode, type QuotaExceededDetails } from './errors'

export type Entry = components['schemas']['Entry']
export type ListResponse = components['schemas']['ListResponse']
export type PutResponse = components['schemas']['PutResponse']
export type ReplicationStatus = components['schemas']['ReplicationStatus']
export type Change = components['schemas']['Change']
export type ChangesResponse = components['schemas']['ChangesResponse']
// STORE-210 (quotas, usage, flags).
export type Usage = components['schemas']['Usage']
export type Quota = components['schemas']['Quota']
export type QuotaUpdate = components['schemas']['QuotaUpdate']
export type TenantSummary = components['schemas']['TenantSummary']
export type TenantList = components['schemas']['TenantList']
export type Flag = components['schemas']['Flag']
export type Flags = components['schemas']['Flags']

/** A successful response: the parsed body, the HTTP status and the ETag (if any). */
export interface ApiResponse<T> {
  data: T
  status: number
  etag: string | null
}

/** Conditional and idempotency headers for writes. */
export interface WriteOptions {
  /** Only write if the key is at exactly this version (sent as If-Match). */
  ifMatch?: number
  /** Only create the key if it does not exist yet (sent as If-None-Match: *). */
  ifNoneMatch?: '*'
  /** Makes a retried request safe: the server applies it at most once. */
  idempotencyKey?: string
}

export interface ClientOptions {
  /** Returns the bearer token, or null when there is none. */
  getToken: () => string | null
  /** Prefix for every path; '' means same origin (the Vite proxy in development). */
  baseUrl?: string
  /** Injected in tests; defaults to the browser's fetch. */
  fetch?: typeof fetch
}

/** Builds the path for a key. Slashes inside a key are kept; every segment is encoded. */
export function keyPath(key: string): string {
  return '/v1/keys/' + key.split('/').map(encodeURIComponent).join('/')
}

/** Reads the version number from an ETag such as "3" (quoted) or 3. */
export function versionFromETag(etag: string | null): number | null {
  if (etag === null) {
    return null
  }
  const n = Number(etag.replace(/^W\//, '').replace(/"/g, ''))
  return Number.isInteger(n) && n > 0 ? n : null
}

/** A fresh idempotency key for one user action. */
export function newIdempotencyKey(): string {
  return crypto.randomUUID()
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

/** Turns a non-2xx response into an ApiError using the standard error envelope. */
async function errorFromResponse(res: Response): Promise<ApiError> {
  const fallback = new ApiError(res.status, 'unknown_error', `Unexpected response (HTTP ${res.status})`)
  let body: unknown
  try {
    body = JSON.parse(await res.text())
  } catch {
    return fallback
  }
  if (!isRecord(body) || !isRecord(body.error)) {
    return fallback
  }
  const { code, message, details } = body.error
  if (typeof code !== 'string' || typeof message !== 'string') {
    return fallback
  }
  return new ApiError(res.status, isErrorCode(code) ? code : 'unknown_error', message, quotaDetails(details))
}

function isCount(v: unknown): v is number {
  return typeof v === 'number' && Number.isInteger(v) && v >= 0
}

/** Reads error.details of a quota_exceeded error; anything malformed is dropped. */
function quotaDetails(v: unknown): QuotaExceededDetails | null {
  if (!isRecord(v)) {
    return null
  }
  const { dimension, used, limit, requested } = v
  if ((dimension !== 'keys' && dimension !== 'bytes') || !isCount(used) || !isCount(limit) || !isCount(requested)) {
    return null
  }
  return { dimension, used, limit, requested }
}

export function createApiClient(options: ClientOptions) {
  const baseUrl = options.baseUrl ?? ''
  const doFetch = options.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => fetch(input, init))

  interface SendOptions {
    body?: unknown
    write?: WriteOptions
    signal?: AbortSignal
    accept?: string
    headers?: Record<string, string>
  }

  async function send(method: string, path: string, opts: SendOptions = {}): Promise<Response> {
    const { body, write = {}, signal } = opts
    const headers = new Headers({ Accept: opts.accept ?? 'application/json', ...opts.headers })
    const token = options.getToken()
    if (token !== null && token !== '') {
      headers.set('Authorization', `Bearer ${token}`)
    }
    if (body !== undefined) {
      headers.set('Content-Type', 'application/json')
    }
    if (write.ifMatch !== undefined) {
      headers.set('If-Match', `"${write.ifMatch}"`)
    }
    if (write.ifNoneMatch !== undefined) {
      headers.set('If-None-Match', write.ifNoneMatch)
    }
    if (write.idempotencyKey !== undefined) {
      headers.set('Idempotency-Key', write.idempotencyKey)
    }

    let res: Response
    try {
      res = await doFetch(baseUrl + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal,
      })
    } catch (e) {
      throw new ApiError(0, 'network_error', e instanceof Error ? e.message : 'network error')
    }
    if (!res.ok) {
      throw await errorFromResponse(res)
    }
    return res
  }

  async function json<T>(res: Response): Promise<ApiResponse<T>> {
    let data: T
    try {
      data = (await res.json()) as T
    } catch {
      throw new ApiError(res.status, 'unknown_error', 'The server sent a response that is not JSON.')
    }
    return { data, status: res.status, etag: res.headers.get('ETag') }
  }

  return {
    /** GET /v1/keys, optionally restricted to keys starting with prefix. */
    async listKeys(prefix = ''): Promise<ApiResponse<ListResponse>> {
      const q = prefix === '' ? '' : `?prefix=${encodeURIComponent(prefix)}`
      return json<ListResponse>(await send('GET', '/v1/keys' + q))
    },

    /** GET /v1/keys/{key}; the ETag carries the version. */
    async getKey(key: string): Promise<ApiResponse<Entry>> {
      return json<Entry>(await send('GET', keyPath(key)))
    },

    /** PUT /v1/keys/{key} with {"value": value}. */
    async putKey(key: string, value: unknown, write: WriteOptions = {}): Promise<ApiResponse<PutResponse>> {
      return json<PutResponse>(await send('PUT', keyPath(key), { body: { value }, write }))
    },

    /** DELETE /v1/keys/{key}; the server answers 204 with no body. */
    async deleteKey(key: string, write: Pick<WriteOptions, 'idempotencyKey'> = {}): Promise<ApiResponse<null>> {
      const res = await send('DELETE', keyPath(key), { write })
      return { data: null, status: res.status, etag: null }
    },

    /**
     * GET /v1/changes: the tenant's changes after `since`. If there are none
     * the server holds the request open (long-poll) until one arrives or its
     * poll timeout passes, then answers with an empty list.
     */
    async changes(since: number, opts: { limit?: number; signal?: AbortSignal } = {}): Promise<ApiResponse<ChangesResponse>> {
      const limit = opts.limit === undefined ? '' : `&limit=${opts.limit}`
      return json<ChangesResponse>(await send('GET', `/v1/changes?since=${since}${limit}`, { signal: opts.signal }))
    },

    /**
     * Opens GET /v1/changes/stream (Server-Sent Events) after `since`, sent as
     * the Last-Event-ID header (never in the URL with the token). Returns the
     * streaming response; read it with response.body.
     */
    async openChangeStream(since: number, signal: AbortSignal): Promise<Response> {
      return send('GET', '/v1/changes/stream', {
        signal,
        accept: 'text/event-stream',
        headers: { 'Last-Event-ID': String(since) },
      })
    },

    /** GET /v1/usage: the signed-in tenant's usage and limits (tenant token). */
    async usage(): Promise<ApiResponse<Usage>> {
      return json<Usage>(await send('GET', '/v1/usage'))
    },

    /** GET /v1/admin/tenants: every tenant with quota and usage (admin token). */
    async listTenants(): Promise<ApiResponse<TenantList>> {
      return json<TenantList>(await send('GET', '/v1/admin/tenants'))
    },

    /** GET /v1/admin/tenants/{id} (admin token). */
    async getTenant(id: number): Promise<ApiResponse<TenantSummary>> {
      return json<TenantSummary>(await send('GET', `/v1/admin/tenants/${id}`))
    },

    /** GET /v1/admin/tenants/{id}/quota (admin token); the ETag is the version. */
    async getQuota(id: number): Promise<ApiResponse<Quota>> {
      return json<Quota>(await send('GET', `/v1/admin/tenants/${id}/quota`))
    },

    /**
     * PUT /v1/admin/tenants/{id}/quota (admin token), only if the quota is still
     * at the given version (If-Match; version 0 = never edited). null = unlimited.
     */
    async putQuota(id: number, update: QuotaUpdate, version: number): Promise<ApiResponse<Quota>> {
      return json<Quota>(await send('PUT', `/v1/admin/tenants/${id}/quota`, { body: update, write: { ifMatch: version } }))
    },

    /** GET /v1/flags (tenant or admin token). */
    async flags(): Promise<ApiResponse<Flags>> {
      return json<Flags>(await send('GET', '/v1/flags'))
    },

    /** PUT /v1/admin/flags/quota_enforcement (admin token), only if the flag is still at the given version. */
    async setQuotaEnforcement(enabled: boolean, version: number): Promise<ApiResponse<Flag>> {
      return json<Flag>(await send('PUT', '/v1/admin/flags/quota_enforcement', { body: { enabled }, write: { ifMatch: version } }))
    },

    /** GET /v1/replication/status (admin token only). */
    async replicationStatus(): Promise<ApiResponse<ReplicationStatus>> {
      return json<ReplicationStatus>(await send('GET', '/v1/replication/status'))
    },
  }
}

export type ApiClient = ReturnType<typeof createApiClient>
