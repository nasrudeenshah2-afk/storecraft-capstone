import { describe, expect, it } from 'vitest'
import { createApiClient, keyPath, versionFromETag } from './client'
import { ApiError } from './errors'
import { errorResponse, jsonResponse, stubFetch } from '../testing/fakeServer'

function client(handler: Parameters<typeof stubFetch>[0], token: string | null = 'secret-token') {
  const stub = stubFetch(handler)
  return { api: createApiClient({ getToken: () => token, fetch: stub.fetch }), requests: stub.requests }
}

async function caught(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p
  } catch (e) {
    if (e instanceof ApiError) return e
    throw e
  }
  throw new Error('expected the call to fail')
}

describe('requests', () => {
  it('sends the bearer token in the Authorization header and never in the URL', async () => {
    const { api, requests } = client(() => jsonResponse(200, { keys: [] }))
    await api.listKeys('app/')
    const req = requests[0]
    expect(req?.headers.get('Authorization')).toBe('Bearer secret-token')
    expect(req?.url).toBe('/v1/keys?prefix=app%2F')
    expect(req?.url).not.toContain('secret-token')
  })

  it('omits the Authorization header when there is no token', async () => {
    const { api, requests } = client(() => jsonResponse(200, { keys: [] }), null)
    await api.listKeys()
    expect(requests[0]?.headers.has('Authorization')).toBe(false)
    expect(requests[0]?.url).toBe('/v1/keys')
  })

  it('sends {"value": ...} as JSON with If-Match, If-None-Match and Idempotency-Key when asked', async () => {
    const { api, requests } = client(() => jsonResponse(200, { key: 'k', version: 4 }, { ETag: '"4"' }))
    const res = await api.putKey('k', { theme: 'dark' }, { ifMatch: 3, idempotencyKey: 'idem-1' })
    const req = requests[0]
    expect(req?.method).toBe('PUT')
    expect(req?.body).toEqual({ value: { theme: 'dark' } })
    expect(req?.headers.get('Content-Type')).toBe('application/json')
    expect(req?.headers.get('If-Match')).toBe('"3"')
    expect(req?.headers.get('Idempotency-Key')).toBe('idem-1')
    expect(req?.headers.has('If-None-Match')).toBe(false)
    expect(res.data).toEqual({ key: 'k', version: 4 })
    expect(res.etag).toBe('"4"')

    await api.putKey('new', 1, { ifNoneMatch: '*' })
    expect(requests[1]?.headers.get('If-None-Match')).toBe('*')
    expect(requests[1]?.headers.has('If-Match')).toBe(false)
  })

  it('reads the ETag of a GET and turns it into a version', async () => {
    const { api } = client(() => jsonResponse(200, { key: 'k', value: 'v', version: 7 }, { ETag: '"7"' }))
    const res = await api.getKey('k')
    expect(res.etag).toBe('"7"')
    expect(versionFromETag(res.etag)).toBe(7)
    expect(versionFromETag('7')).toBe(7)
    expect(versionFromETag(null)).toBeNull()
    expect(versionFromETag('"abc"')).toBeNull()
  })

  it('handles a 204 delete with no body', async () => {
    const { api, requests } = client(() => new Response(null, { status: 204 }))
    const res = await api.deleteKey('a b/c', { idempotencyKey: 'd1' })
    expect(res).toEqual({ data: null, status: 204, etag: null })
    expect(requests[0]?.method).toBe('DELETE')
    expect(requests[0]?.url).toBe('/v1/keys/a%20b/c')
    expect(requests[0]?.body).toBeUndefined()
  })

  it('encodes each key segment but keeps slashes', () => {
    expect(keyPath('app/config/theme')).toBe('/v1/keys/app/config/theme')
    expect(keyPath('a?b#c d')).toBe('/v1/keys/a%3Fb%23c%20d')
    expect(keyPath('ümlaut')).toBe('/v1/keys/%C3%BCmlaut')
  })
})

describe('error mapping', () => {
  const cases: Array<[number, string]> = [
    [400, 'invalid_request'],
    [401, 'unauthorized'],
    [403, 'forbidden'],
    [404, 'not_found'],
    [405, 'method_not_allowed'],
    [409, 'read_only_replica'],
    [412, 'precondition_failed'],
    [422, 'idempotency_conflict'],
    [500, 'internal'],
  ]
  it.each(cases)('maps a %i envelope with code %s to an ApiError', async (status, code) => {
    const { api } = client(() => errorResponse(status, code, `server says ${code}`))
    const err = await caught(api.getKey('k'))
    expect(err.status).toBe(status)
    expect(err.code).toBe(code)
    expect(err.message).toBe(`server says ${code}`)
  })

  it('maps an unknown envelope code to unknown_error but keeps the message', async () => {
    const { api } = client(() => errorResponse(418, 'teapot', 'short and stout'))
    const err = await caught(api.getKey('k'))
    expect(err).toMatchObject({ status: 418, code: 'unknown_error', message: 'short and stout' })
  })

  it('maps a non-JSON error body (for example from a proxy) to unknown_error', async () => {
    const { api } = client(() => new Response('<html>Bad Gateway</html>', { status: 502 }))
    const err = await caught(api.listKeys())
    expect(err).toMatchObject({ status: 502, code: 'unknown_error' })
  })

  it('maps a JSON error without the envelope shape to unknown_error', async () => {
    const { api } = client(() => jsonResponse(500, { oops: true }))
    expect((await caught(api.listKeys())).code).toBe('unknown_error')
  })

  it('maps a network failure to status 0 and network_error', async () => {
    const { api } = client(() => {
      throw new TypeError('Failed to fetch')
    })
    const err = await caught(api.listKeys())
    expect(err).toMatchObject({ status: 0, code: 'network_error' })
  })

  it('maps a 200 with a body that is not JSON to unknown_error', async () => {
    const { api } = client(() => new Response('not json', { status: 200 }))
    expect((await caught(api.listKeys())).code).toBe('unknown_error')
  })
})

describe('STORE-210 calls (contract stubs, step B3)', () => {
  it('reads usage, tenants, quota and flags from the documented paths', async () => {
    const { api, requests } = client((req) => {
      if (req.url === '/v1/usage') {
        return jsonResponse(200, { tenant_id: 1, keys: { used: 2, limit: 1000 }, bytes: { used: 7, limit: null }, seq: 9 })
      }
      if (req.url === '/v1/admin/tenants') {
        return jsonResponse(200, { tenants: [] })
      }
      if (req.url === '/v1/admin/tenants/3') {
        return jsonResponse(200, { id: 3, name: 't', quota: { max_keys: 1000, max_bytes: 1048576, version: 0 }, usage: { keys: 0, bytes: 0 } })
      }
      if (req.url === '/v1/admin/tenants/3/quota') {
        return jsonResponse(200, { max_keys: 1000, max_bytes: 1048576, version: 0 }, { ETag: '"0"' })
      }
      return jsonResponse(200, { quota_enforcement: { enabled: false, version: 0 } })
    })
    expect((await api.usage()).data.bytes.limit).toBeNull()
    expect((await api.listTenants()).data.tenants).toEqual([])
    expect((await api.getTenant(3)).data.name).toBe('t')
    const quota = await api.getQuota(3)
    expect(quota.data.version).toBe(0)
    expect(quota.etag).toBe('"0"')
    expect((await api.flags()).data.quota_enforcement.enabled).toBe(false)
    expect(requests.map((r) => `${r.method} ${r.url}`)).toEqual([
      'GET /v1/usage',
      'GET /v1/admin/tenants',
      'GET /v1/admin/tenants/3',
      'GET /v1/admin/tenants/3/quota',
      'GET /v1/flags',
    ])
  })

  it('sends quota and flag edits with If-Match, including version 0', async () => {
    const { api, requests } = client((req) =>
      req.url.endsWith('/quota')
        ? jsonResponse(200, { max_keys: 5, max_bytes: null, version: 1 }, { ETag: '"1"' })
        : jsonResponse(200, { enabled: true, version: 1 }, { ETag: '"1"' }),
    )
    const q = await api.putQuota(3, { max_keys: 5, max_bytes: null }, 0)
    expect(q.data).toEqual({ max_keys: 5, max_bytes: null, version: 1 })
    const f = await api.setQuotaEnforcement(true, 0)
    expect(f.data.enabled).toBe(true)

    expect(requests[0]).toMatchObject({ method: 'PUT', url: '/v1/admin/tenants/3/quota', body: { max_keys: 5, max_bytes: null } })
    expect(requests[0]?.headers.get('If-Match')).toBe('"0"')
    expect(requests[1]).toMatchObject({ method: 'PUT', url: '/v1/admin/flags/quota_enforcement', body: { enabled: true } })
    expect(requests[1]?.headers.get('If-Match')).toBe('"0"')
  })

  it('keeps quota_exceeded details from the error envelope', async () => {
    const { api } = client(() =>
      jsonResponse(403, {
        error: {
          code: 'quota_exceeded',
          message: 'This write needs 2048 more bytes, but 1047000 of 1048576 bytes are used.',
          details: { dimension: 'bytes', used: 1047000, limit: 1048576, requested: 2048 },
        },
      }),
    )
    const err = await caught(api.putKey('k', 'big'))
    expect(err.status).toBe(403)
    expect(err.code).toBe('quota_exceeded')
    expect(err.details).toEqual({ dimension: 'bytes', used: 1047000, limit: 1048576, requested: 2048 })
  })

  it('drops malformed details and has none for other errors', async () => {
    for (const details of [{ dimension: 'cats', used: 1, limit: 1, requested: 1 }, { dimension: 'keys', used: -1, limit: 1, requested: 1 }, 'text', null]) {
      const { api } = client(() => jsonResponse(403, { error: { code: 'quota_exceeded', message: 'x', details } }))
      expect((await caught(api.putKey('k', 1))).details).toBeNull()
    }
    const { api } = client(() => errorResponse(403, 'forbidden', 'admin token cannot access tenant data'))
    expect((await caught(api.putKey('k', 1))).details).toBeNull()
  })
})
