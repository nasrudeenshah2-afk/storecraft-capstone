/** One Server-Sent Event: the fields the storecraft stream uses. */
export interface SSEEvent {
  id: string | null
  event: string
  data: string
}

/**
 * Incremental parser for the text/event-stream format. Feed it decoded text in
 * whatever pieces the network delivers (an event may be split across chunks,
 * or several may arrive in one); it returns the events completed so far.
 * Comment lines (starting with ":", used for heartbeats) are ignored.
 */
export class SSEParser {
  private buffer = ''
  private id: string | null = null
  private event = ''
  private data: string[] = []
  private hasField = false
  /** The previous chunk ended with "\r": a "\n" at the start of this one belongs to it. */
  private skipLF = false

  push(text: string): SSEEvent[] {
    if (this.skipLF && text.startsWith('\n')) {
      text = text.slice(1)
    }
    this.skipLF = false
    this.buffer += text
    const out: SSEEvent[] = []
    for (;;) {
      const nl = this.buffer.search(/\r\n|\r|\n/)
      if (nl < 0) {
        break
      }
      const sepLen = this.buffer.startsWith('\r\n', nl) ? 2 : 1
      // A "\r" ends the line right away; if it is the last character, the
      // "\n" of a "\r\n" pair may arrive at the start of the next chunk.
      if (this.buffer[nl] === '\r' && sepLen === 1 && nl === this.buffer.length - 1) {
        this.skipLF = true
      }
      const line = this.buffer.slice(0, nl)
      this.buffer = this.buffer.slice(nl + sepLen)
      const ev = this.line(line)
      if (ev !== null) {
        out.push(ev)
      }
    }
    return out
  }

  private line(line: string): SSEEvent | null {
    if (line === '') {
      // A blank line ends the event (if it had any field).
      if (!this.hasField) {
        return null
      }
      const ev: SSEEvent = { id: this.id, event: this.event === '' ? 'message' : this.event, data: this.data.join('\n') }
      this.event = ''
      this.data = []
      this.hasField = false
      // The id persists across events, as in the SSE spec.
      return ev
    }
    if (line.startsWith(':')) {
      return null // comment / heartbeat
    }
    const colon = line.indexOf(':')
    const field = colon < 0 ? line : line.slice(0, colon)
    let value = colon < 0 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) {
      value = value.slice(1)
    }
    switch (field) {
      case 'id':
        this.id = value
        this.hasField = true
        break
      case 'event':
        this.event = value
        this.hasField = true
        break
      case 'data':
        this.data.push(value)
        this.hasField = true
        break
      default:
        // Unknown fields (for example "retry") are ignored.
        break
    }
    return null
  }
}
