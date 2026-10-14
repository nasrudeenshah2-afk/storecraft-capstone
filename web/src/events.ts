// Client events (STORE-210, spec section 9). They are recorded in memory and
// dispatched in the browser as a "storecraft:client-event" CustomEvent, for
// tests and future telemetry. They are NOT sent anywhere (a non-goal), and
// their properties never contain a token or a key's value.

export type ClientEventName =
  | 'quota_rejected_shown'
  | 'quota_editor_saved'
  | 'quota_editor_conflict'
  | 'quota_flag_toggled'
  | 'usage_near_limit_shown'

export interface ClientEvent {
  name: ClientEventName
  props: Readonly<Record<string, string | number | boolean>>
  at: number
}

export const CLIENT_EVENT = 'storecraft:client-event'
const MAX_RECORDED = 200
const recorded: ClientEvent[] = []

/** Records one event. */
export function track(name: ClientEventName, props: Record<string, string | number | boolean> = {}): void {
  const event: ClientEvent = { name, props: { ...props }, at: Date.now() }
  recorded.push(event)
  if (recorded.length > MAX_RECORDED) {
    recorded.shift()
  }
  window.dispatchEvent(new CustomEvent<ClientEvent>(CLIENT_EVENT, { detail: event }))
}

/** The events recorded so far (oldest first). */
export function recordedEvents(): readonly ClientEvent[] {
  return recorded
}

export function clearRecordedEvents(): void {
  recorded.length = 0
}
