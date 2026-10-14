import { describe, expect, it } from 'vitest'
import { SSEParser } from './sse'

describe('SSEParser', () => {
  it('parses complete events and ignores heartbeat comments', () => {
    const p = new SSEParser()
    const events = p.push(': heartbeat\n\nid: 5\nevent: change\ndata: {"seq":5}\n\n: heartbeat\n\n')
    expect(events).toEqual([{ id: '5', event: 'change', data: '{"seq":5}' }])
  })

  it('joins events split across arbitrary chunks, including inside a line', () => {
    const text = 'id: 7\nevent: change\ndata: {"key":"a b"}\n\nid: 8\nevent: change\ndata: {"key":"c"}\n\n'
    for (let size = 1; size <= 9; size++) {
      const p = new SSEParser()
      const got = []
      for (let i = 0; i < text.length; i += size) {
        got.push(...p.push(text.slice(i, i + size)))
      }
      expect(got.map((e) => e.id), `chunk size ${size}`).toEqual(['7', '8'])
      expect(got[0]?.data).toBe('{"key":"a b"}')
    }
  })

  it('accepts CRLF and CR line endings, also split between chunks', () => {
    const p = new SSEParser()
    const got = [...p.push('id: 1\r'), ...p.push('\ndata: x\r\n\r'), ...p.push('\n'), ...p.push('id: 2\rdata: y\r\r')]
    expect(got.map((e) => [e.id, e.data])).toEqual([
      ['1', 'x'],
      ['2', 'y'],
    ])
  })

  it('joins multi-line data, defaults the event type, keeps the last id', () => {
    const p = new SSEParser()
    expect(p.push('id: 3\ndata: one\ndata: two\n\ndata: three\n\n')).toEqual([
      { id: '3', event: 'message', data: 'one\ntwo' },
      { id: '3', event: 'message', data: 'three' },
    ])
  })

  it('ignores unknown fields and empty events', () => {
    const p = new SSEParser()
    expect(p.push('retry: 1000\n\n\n\nfoo: bar\ndata:no-space\n\n')).toEqual([{ id: null, event: 'message', data: 'no-space' }])
  })
})
