import { describe, expect, it, vi } from 'vitest'
import { createApiClient } from '../api/client'
import { fakeServer } from '../testing/fakeServer'
import { parseJsonValue, previewValue, useKeys, validateKeyName } from './useKeys'

function setup(options: Parameters<typeof fakeServer>[0] = {}, token?: string) {
  const server = fakeServer(options)
  const api = createApiClient({ getToken: () => token ?? server.tenantToken, fetch: server.fetch })
  const onUnauthorized = vi.fn()
  return { server, keys: useKeys(api, onUnauthorized), onUnauthorized }
}

describe('useKeys: listing', () => {
  it('loads keys sorted by name and remembers the applied prefix filter', async () => {
    const { server, keys } = setup()
    server.externalPut('b', 2)
    server.externalPut('app/x', 'x')
    server.externalPut('a', 1)
    await keys.load()
    expect(keys.items.value.map((e) => e.key)).toEqual(['a', 'app/x', 'b'])

    keys.prefix.value = 'app/'
    await keys.load()
    expect(keys.items.value.map((e) => e.key)).toEqual(['app/x'])
    expect(keys.appliedPrefix.value).toBe('app/')
    expect(server.requests.at(-1)?.url).toBe('/v1/keys?prefix=app%2F')

    await keys.clearFilter()
    expect(keys.items.value).toHaveLength(3)
    expect(keys.appliedPrefix.value).toBe('')
  })

  it('shows a clear message when an admin token tries to list keys', async () => {
    const { server, keys } = setup({}, 'admin-token')
    await keys.load()
    expect(server.requests).toHaveLength(1)
    expect(keys.loadError.value).toContain('tenant token')
    expect(keys.items.value).toEqual([])
  })

  it('calls onUnauthorized when the token is rejected', async () => {
    const { keys, onUnauthorized } = setup({}, 'wrong-token')
    await keys.load()
    expect(onUnauthorized).toHaveBeenCalledOnce()
    expect(keys.loadError.value).toContain('Sign in again')
  })
})

describe('useKeys: create', () => {
  it('creates with If-None-Match: * and an Idempotency-Key, then reloads', async () => {
    const { server, keys } = setup()
    const r = await keys.create('app/theme', '{"mode": "dark"}')
    expect(r).toEqual({ ok: true })
    const put = server.requests.find((q) => q.method === 'PUT')
    expect(put?.url).toBe('/v1/keys/app/theme')
    expect(put?.headers.get('If-None-Match')).toBe('*')
    expect(put?.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/)
    expect(put?.body).toEqual({ value: { mode: 'dark' } })
    expect(keys.items.value).toEqual([{ key: 'app/theme', value: { mode: 'dark' }, version: 1 }])
  })

  it('does not overwrite an existing key: it reports "already exists"', async () => {
    const { server, keys } = setup()
    server.externalPut('k', 'original')
    const r = await keys.create('k', '"replacement"')
    expect(r).toMatchObject({ ok: false, code: 'precondition_failed' })
    expect(!r.ok && r.message).toContain('already exists')
    expect(server.data.get('k')?.value).toBe('original')
  })

  it('validates the key name and the JSON before sending anything', async () => {
    const { server, keys } = setup()
    expect(await keys.create('', '1')).toMatchObject({ ok: false, code: 'validation', field: 'key' })
    expect(await keys.create('a//b', '1')).toMatchObject({ ok: false, code: 'validation', field: 'key' })
    expect(await keys.create('k', 'hello')).toMatchObject({ ok: false, code: 'validation', field: 'value' })
    expect(await keys.create('k', '   ')).toMatchObject({ ok: false, code: 'validation', field: 'value' })
    expect(server.requests).toHaveLength(0)
  })

  it('uses a different Idempotency-Key for each action', async () => {
    const { server, keys } = setup()
    await keys.create('a', '1')
    await keys.create('b', '2')
    const ids = server.requests.filter((q) => q.method === 'PUT').map((q) => q.headers.get('Idempotency-Key'))
    expect(new Set(ids).size).toBe(2)
  })
})

describe('useKeys: edit', () => {
  it('sends If-Match with the version that was shown', async () => {
    const { server, keys } = setup()
    server.externalPut('k', 'v1')
    server.externalPut('k', 'v2') // now at version 2
    await keys.load()
    const shown = keys.items.value[0]
    expect(shown?.version).toBe(2)
    if (shown === undefined) return

    const r = await keys.save(shown, '"v3"')
    expect(r).toEqual({ ok: true })
    const put = server.requests.find((q) => q.method === 'PUT')
    expect(put?.headers.get('If-Match')).toBe('"2"')
    expect(put?.headers.has('If-None-Match')).toBe(false)
    expect(keys.items.value[0]).toEqual({ key: 'k', value: 'v3', version: 3 })
  })

  it('reports a conflict (412) when someone else changed the key, and changes nothing', async () => {
    const { server, keys } = setup()
    server.externalPut('k', 'mine')
    await keys.load()
    const shown = keys.items.value[0]
    if (shown === undefined) throw new Error('no key loaded')
    server.externalPut('k', 'theirs') // another client wins the race

    const r = await keys.save(shown, '"my edit"')
    expect(r).toMatchObject({ ok: false, code: 'precondition_failed' })
    expect(!r.ok && r.message).toContain('version 1')
    expect(server.data.get('k')).toEqual({ value: 'theirs', version: 2 })
  })

  it('maps a read-only replica (409) to its own message', async () => {
    const { server, keys } = setup({ readOnly: true })
    server.externalPut('k', 1)
    await keys.load()
    const shown = keys.items.value[0]
    if (shown === undefined) throw new Error('no key loaded')
    const r = await keys.save(shown, '2')
    expect(r).toMatchObject({ ok: false, code: 'read_only_replica' })
    expect(!r.ok && r.message).toContain('read-only replica')
  })
})

describe('useKeys: delete', () => {
  it('deletes and reloads', async () => {
    const { server, keys } = setup()
    server.externalPut('k', 1)
    server.externalPut('other', 2)
    expect(await keys.remove('k')).toEqual({ ok: true })
    expect(server.requests.find((q) => q.method === 'DELETE')?.url).toBe('/v1/keys/k')
    expect(keys.items.value.map((e) => e.key)).toEqual(['other'])
  })

  it('reports a key that is already gone', async () => {
    const { keys } = setup()
    const r = await keys.remove('ghost')
    expect(r).toMatchObject({ ok: false, code: 'not_found' })
    expect(!r.ok && r.message).toContain('already deleted')
  })

  it('reports a network failure', async () => {
    const api = createApiClient({
      getToken: () => 't',
      fetch: () => Promise.reject(new TypeError('Failed to fetch')),
    })
    const r = await useKeys(api, vi.fn()).remove('k')
    expect(r).toMatchObject({ ok: false, code: 'network_error' })
  })
})

describe('helpers', () => {
  it('validateKeyName', () => {
    expect(validateKeyName('app/theme')).toBeNull()
    expect(validateKeyName('with space')).toBeNull()
    for (const bad of ['', '/lead', 'trail/', 'a//b', '.', 'a/../b', 'a/./b']) {
      expect(validateKeyName(bad), bad).not.toBeNull()
    }
  })

  it('parseJsonValue accepts any JSON value', () => {
    for (const ok of ['"text"', '42', 'true', 'null', '[1,2]', '{"a": {"b": 1}}']) {
      expect(parseJsonValue(ok).ok, ok).toBe(true)
    }
    expect(parseJsonValue("{'a': 1}").ok).toBe(false)
  })

  it('previewValue shortens long values', () => {
    expect(previewValue({ a: 1 })).toBe('{"a":1}')
    const long = previewValue('x'.repeat(200))
    expect(long.length).toBe(80)
    expect(long.endsWith('…')).toBe(true)
  })
})

describe('useKeys: live changes', () => {
  function change(seq: number, key: string, op: 'put' | 'delete', version: number, value: unknown = null) {
    return { seq, key, op, version, value: op === 'put' ? value : null, value_size: 0, ts: new Date(0).toISOString() }
  }

  it('applies changes newer than the list and ignores ones the list already contains', async () => {
    const { server, keys } = setup()
    server.externalPut('a', 'one') // seq 1
    await keys.load()
    expect(keys.listSeq.value).toBe(1)

    keys.applyChange(change(1, 'a', 'put', 1, 'stale replay')) // already in the list
    expect(keys.items.value).toEqual([{ key: 'a', value: 'one', version: 1 }])

    keys.applyChange(change(2, 'b', 'put', 1, 'new'))
    keys.applyChange(change(3, 'a', 'put', 2, 'two'))
    expect(keys.items.value).toEqual([
      { key: 'a', value: 'two', version: 2 },
      { key: 'b', value: 'new', version: 1 },
    ])
  })

  it('orders by seq, so an old delete never removes a key re-created later (versions restart at 1)', async () => {
    const { keys } = setup()
    await keys.load() // empty, seq 0
    keys.applyChange(change(1, 'k', 'put', 1, 'first life'))
    keys.applyChange(change(2, 'k', 'put', 2, 'still first'))
    keys.applyChange(change(3, 'k', 'delete', 2))
    keys.applyChange(change(4, 'k', 'put', 1, 'second life')) // version 1 again
    expect(keys.items.value).toEqual([{ key: 'k', value: 'second life', version: 1 }])

    // A late, older change for the same key (seq 3 again) must not undo it.
    keys.applyChange(change(3, 'k', 'delete', 2))
    expect(keys.items.value).toEqual([{ key: 'k', value: 'second life', version: 1 }])
  })

  it('ignores changes outside the current prefix filter', async () => {
    const { keys } = setup()
    keys.prefix.value = 'app/'
    await keys.load()
    keys.applyChange(change(1, 'other', 'put', 1, 'x'))
    keys.applyChange(change(2, 'app/theme', 'put', 1, 'dark'))
    expect(keys.items.value.map((e) => e.key)).toEqual(['app/theme'])
  })

  it('keeps changes that arrive while a reload is in flight', async () => {
    const { server, keys } = setup()
    server.externalPut('a', 1) // seq 1
    await keys.load()
    const reload = keys.load() // the snapshot is taken now (seq 1)...
    keys.applyChange(change(2, 'late', 'put', 1, 'arrived during the reload')) // ...and this arrives meanwhile
    await reload
    // The fake server answered the reload with its snapshot (seq 1); the change at seq 2 must survive it.
    expect(keys.items.value.map((e) => e.key)).toEqual(['a', 'late'])
  })
})
