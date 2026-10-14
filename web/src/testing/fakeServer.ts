// Test-only helper (never imported by the app): an in-memory imitation of the
// storecraft key API, served through a stubbed fetch. It follows the real
// server's rules from api/openapi.yaml: bearer auth, versions, If-Match,
// If-None-Match: *, 204 on delete, the standard error envelope, a change log
// with seq numbers, the long-poll /v1/changes and the SSE /v1/changes/stream.

export interface RecordedRequest {
  method: string
  url: string
  headers: Headers
  body: unknown
  signal: AbortSignal | null
}

export function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

export function errorResponse(status: number, code: string, message: string): Response {
  return jsonResponse(status, { error: { code, message } })
}

/** A fetch stub that records every request and answers with handler. */
export function stubFetch(handler: (req: RecordedRequest) => Response | Promise<Response>) {
  const requests: RecordedRequest[] = []
  const fetchFn = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    const rawBody = init?.body
    const req: RecordedRequest = {
      method: init?.method ?? 'GET',
      url,
      headers: new Headers(init?.headers),
      body: typeof rawBody === 'string' ? (JSON.parse(rawBody) as unknown) : undefined,
      signal: init?.signal ?? null,
    }
    requests.push(req)
    if (req.signal?.aborted === true) {
      throw new DOMException('The operation was aborted.', 'AbortError')
    }
    return handler(req)
  }
  return { fetch: fetchFn as typeof fetch, requests }
}

interface StoredKey {
  value: unknown
  version: number
}

interface LoggedChange {
  seq: number
  key: string
  op: 'put' | 'delete'
  version: number
  value: unknown
  value_size: number
  ts: string
}

export interface FakeServerOptions {
  /** The tenant token the server accepts. */
  tenantToken?: string
  /** The admin token the server accepts. */
  adminToken?: string
  /** Behave like a read-only replica (writes get 409). */
  readOnly?: boolean
  /** How long an idle long-poll waits before answering with no changes. */
  pollTimeoutMs?: number
}

/** An in-memory storecraft server for one tenant. */
export function fakeServer(options: FakeServerOptions = {}) {
  const tenantToken = options.tenantToken ?? 'tenant-token'
  const adminToken = options.adminToken ?? 'admin-token'
  const pollTimeoutMs = options.pollTimeoutMs ?? 1000
  const data = new Map<string, StoredKey>()
  const log: LoggedChange[] = []
  const streams = new Set<ReadableStreamDefaultController<Uint8Array>>()
  const waiters = new Set<() => void>()
  const encoder = new TextEncoder()
  let seq = 0
  const control = { streamFailures: 0, blockStream: false }

  function sseEvent(c: LoggedChange): Uint8Array {
    return encoder.encode(`id: ${c.seq}\nevent: change\ndata: ${JSON.stringify(c)}\n\n`)
  }

  function record(key: string, op: 'put' | 'delete', version: number, value: unknown): void {
    seq += 1
    const json = op === 'put' ? JSON.stringify(value) : ''
    const c: LoggedChange = {
      seq,
      key,
      op,
      version,
      value: op === 'put' ? value : null,
      value_size: json.length,
      ts: new Date(0).toISOString(),
    }
    log.push(c)
    for (const ctrl of streams) {
      ctrl.enqueue(sseEvent(c))
    }
    for (const wake of [...waiters]) {
      wake()
    }
  }

  function put(key: string, value: unknown): number {
    const version = (data.get(key)?.version ?? 0) + 1
    data.set(key, { value, version })
    record(key, 'put', version, value)
    return version
  }

  function remove(key: string): boolean {
    const current = data.get(key)
    if (current === undefined) {
      return false
    }
    data.delete(key)
    record(key, 'delete', current.version, null)
    return true
  }

  function changesAfter(since: number): LoggedChange[] {
    return log.filter((c) => c.seq > since)
  }

  function openStream(since: number, signal: AbortSignal | null): Response {
    let ctrl: ReadableStreamDefaultController<Uint8Array> | null = null
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        ctrl = c
        streams.add(c)
        for (const ch of changesAfter(since)) {
          c.enqueue(sseEvent(ch))
        }
      },
      cancel() {
        if (ctrl !== null) streams.delete(ctrl)
      },
    })
    signal?.addEventListener('abort', () => {
      if (ctrl !== null && streams.delete(ctrl)) {
        try {
          ctrl.error(new DOMException('The operation was aborted.', 'AbortError'))
        } catch {
          // already closed
        }
      }
    })
    return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
  }

  async function longPoll(since: number, signal: AbortSignal | null): Promise<Response> {
    let found = changesAfter(since)
    if (found.length === 0) {
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer)
          waiters.delete(done)
          resolve()
        }
        const timer = setTimeout(done, pollTimeoutMs)
        waiters.add(done)
        signal?.addEventListener('abort', done)
      })
      if (signal?.aborted === true) {
        throw new DOMException('The operation was aborted.', 'AbortError')
      }
      found = changesAfter(since)
    }
    const last = found.length > 0 ? (found[found.length - 1]?.seq ?? since) : since
    return jsonResponse(200, { changes: found, last_seq: last })
  }

  async function route(req: RecordedRequest): Promise<Response> {
    const url = new URL(req.url, 'http://test.local')
    const auth = req.headers.get('Authorization')
    const token = auth?.startsWith('Bearer ') === true ? auth.slice('Bearer '.length) : ''

    if (url.pathname === '/v1/replication/status') {
      if (token === adminToken) {
        return jsonResponse(200, { role: 'primary', applied_seq: seq, primary_seq: seq, lag: 0 })
      }
      return token === tenantToken
        ? errorResponse(403, 'forbidden', 'this route requires the admin token')
        : errorResponse(401, 'unauthorized', 'missing or invalid bearer token')
    }
    if (token === adminToken) {
      return errorResponse(403, 'forbidden', 'admin token cannot access tenant data')
    }
    if (token !== tenantToken) {
      return errorResponse(401, 'unauthorized', 'missing or invalid bearer token')
    }

    if (url.pathname === '/v1/keys' && req.method === 'GET') {
      const prefix = url.searchParams.get('prefix') ?? ''
      const keys = [...data.entries()]
        .filter(([k]) => k.startsWith(prefix))
        .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
        .map(([key, s]) => ({ key, value: s.value, version: s.version }))
      return jsonResponse(200, { keys, seq })
    }

    if (url.pathname === '/v1/changes' && req.method === 'GET') {
      return longPoll(Number(url.searchParams.get('since') ?? '0'), req.signal)
    }

    if (url.pathname === '/v1/changes/stream' && req.method === 'GET') {
      if (control.blockStream) {
        return errorResponse(503, 'internal', 'stream unavailable')
      }
      if (control.streamFailures > 0) {
        control.streamFailures -= 1
        return errorResponse(503, 'internal', 'stream unavailable')
      }
      const since = Number(req.headers.get('Last-Event-ID') ?? url.searchParams.get('since') ?? '0')
      return openStream(since, req.signal)
    }

    if (url.pathname.startsWith('/v1/keys/')) {
      const key = url.pathname.slice('/v1/keys/'.length).split('/').map(decodeURIComponent).join('/')
      const current = data.get(key)
      if (req.method === 'GET') {
        return current === undefined
          ? errorResponse(404, 'not_found', 'key not found')
          : jsonResponse(200, { key, value: current.value, version: current.version }, { ETag: `"${current.version}"` })
      }
      if (options.readOnly === true && (req.method === 'PUT' || req.method === 'DELETE')) {
        return errorResponse(409, 'read_only_replica', 'this node is a read-only replica')
      }
      if (req.method === 'PUT') {
        const ifMatch = req.headers.get('If-Match')
        const ifNoneMatch = req.headers.get('If-None-Match')
        if (ifNoneMatch === '*' && current !== undefined) {
          return errorResponse(412, 'precondition_failed', 'version precondition failed')
        }
        if (ifMatch !== null && (current === undefined || `"${current.version}"` !== ifMatch)) {
          return errorResponse(412, 'precondition_failed', 'version precondition failed')
        }
        const body = req.body as { value?: unknown } | undefined
        if (body === undefined || !('value' in body)) {
          return errorResponse(400, 'invalid_request', 'missing "value" field')
        }
        const version = put(key, body.value)
        return jsonResponse(version === 1 ? 201 : 200, { key, version }, { ETag: `"${version}"` })
      }
      if (req.method === 'DELETE') {
        return remove(key) ? new Response(null, { status: 204 }) : errorResponse(404, 'not_found', 'key not found')
      }
      return errorResponse(405, 'method_not_allowed', 'method not allowed')
    }
    return errorResponse(404, 'not_found', 'no such route')
  }

  const stub = stubFetch(route)
  return {
    fetch: stub.fetch,
    requests: stub.requests,
    data,
    log,
    tenantToken,
    adminToken,
    control,
    /** Simulates another client writing the key (bumps its version, logs a change). */
    externalPut(key: string, value: unknown): number {
      return put(key, value)
    },
    /** Simulates another client deleting the key. */
    externalDelete(key: string): void {
      remove(key)
    },
    /** Number of currently open SSE streams. */
    openStreams(): number {
      return streams.size
    },
    /** Simulates the server or network dropping every open stream. */
    dropStreams(): void {
      for (const ctrl of [...streams]) {
        streams.delete(ctrl)
        ctrl.error(new TypeError('network connection lost'))
      }
    },
    /** Ends every open stream cleanly (as a server restart would). */
    closeStreams(): void {
      for (const ctrl of [...streams]) {
        streams.delete(ctrl)
        ctrl.close()
      }
    },
  }
}
