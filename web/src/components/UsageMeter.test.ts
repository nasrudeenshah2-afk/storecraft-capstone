import { mount } from '@vue/test-utils'
import { afterEach, describe, expect, it } from 'vitest'
import type { Usage } from '../api/client'
import { clearRecordedEvents, recordedEvents } from '../events'
import UsageMeter from './UsageMeter.vue'

function usage(keys: [number, number | null], bytes: [number, number | null]): Usage {
  return { tenant_id: 1, keys: { used: keys[0], limit: keys[1] }, bytes: { used: bytes[0], limit: bytes[1] }, seq: 1 }
}

describe('UsageMeter (AC-UI-1)', () => {
  afterEach(() => clearRecordedEvents())

  it('loading', () => {
    const w = mount(UsageMeter, { props: { usage: null, status: 'loading', error: '' } })
    expect(w.text()).toContain('Loading usage…')
    expect(w.find('[role="meter"]').exists()).toBe(false)
  })

  it('error, with a Try again button that asks to retry', async () => {
    const w = mount(UsageMeter, { props: { usage: null, status: 'error', error: 'The server had an unexpected problem.' } })
    expect(w.get('[role="alert"]').text()).toContain('Could not load usage')
    await w.get('button').trigger('click')
    expect(w.emitted('retry')).toHaveLength(1)
  })

  it('normal: text plus an accessible meter for each dimension', () => {
    const w = mount(UsageMeter, { props: { usage: usage([12, 1000], [3482, 1048576]), status: 'ready', error: '' } })
    expect(w.text()).toContain('12 of 1,000 keys')
    expect(w.text()).toContain('3.4 KB of 1 MB')
    const meters = w.findAll('[role="meter"]')
    expect(meters.map((m) => m.attributes('aria-label'))).toEqual(['Keys used', 'Bytes used'])
    expect(meters[0]?.attributes()).toMatchObject({ 'aria-valuenow': '12', 'aria-valuemax': '1000', 'aria-valuemin': '0', 'aria-valuetext': '12 of 1,000 keys' })
    expect(w.text()).not.toMatch(/Almost full|Over quota/)
  })

  it('unlimited: the word, and no meter value', () => {
    const w = mount(UsageMeter, { props: { usage: usage([3, null], [10, null]), status: 'ready', error: '' } })
    expect(w.text()).toContain('3 keys (unlimited)')
    expect(w.text()).toContain('10 bytes (unlimited)')
    expect(w.findAll('[role="meter"]')).toHaveLength(0)
  })

  it('near limit and over quota are said in words, not colour alone', async () => {
    const w = mount(UsageMeter, { props: { usage: usage([850, 1000], [1, 100]), status: 'ready', error: '' } })
    expect(w.text()).toContain('Almost full')
    expect(recordedEvents().map((e) => [e.name, e.props.dimension])).toEqual([['usage_near_limit_shown', 'keys']])

    await w.setProps({ usage: usage([860, 1000], [1, 100]) }) // still near: no second event
    expect(recordedEvents()).toHaveLength(1)

    await w.setProps({ usage: usage([1200, 1000], [2097152, 1048576]) })
    expect(w.text()).toContain('Over quota')
    expect(w.text()).toContain('2 MB of 1 MB')
  })
})
