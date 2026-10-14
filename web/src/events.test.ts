import { afterEach, describe, expect, it } from 'vitest'
import { CLIENT_EVENT, clearRecordedEvents, recordedEvents, track, type ClientEvent } from './events'

describe('client events', () => {
  afterEach(() => clearRecordedEvents())

  it('records events and dispatches them in the browser', () => {
    const seen: ClientEvent[] = []
    const listener = (e: Event) => seen.push((e as CustomEvent<ClientEvent>).detail)
    window.addEventListener(CLIENT_EVENT, listener)
    track('quota_editor_saved', { tenant_id: 3, version: 2 })
    window.removeEventListener(CLIENT_EVENT, listener)

    expect(recordedEvents().map((e) => [e.name, e.props])).toEqual([['quota_editor_saved', { tenant_id: 3, version: 2 }]])
    expect(seen).toHaveLength(1)
    expect(seen[0]?.name).toBe('quota_editor_saved')
  })

  it('keeps only the newest 200 events', () => {
    for (let i = 0; i < 250; i++) track('quota_flag_toggled', { enabled: i % 2 === 0 })
    expect(recordedEvents()).toHaveLength(200)
  })
})
