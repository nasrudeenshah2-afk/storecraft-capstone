import { mount } from '@vue/test-utils'
import { afterEach, describe, expect, it } from 'vitest'
import { nextTick } from 'vue'
import ModalHost from '../testing/ModalHost.vue'

describe('BaseModal', () => {
  afterEach(() => {
    document.body.innerHTML = ''
  })

  async function openModal() {
    const wrapper = mount(ModalHost, { attachTo: document.body })
    const opener = wrapper.get('#opener')
    ;(opener.element as HTMLElement).focus()
    await opener.trigger('click')
    await nextTick()
    await nextTick()
    return wrapper
  }

  it('is a labelled modal dialog and focuses its first control', async () => {
    const wrapper = await openModal()
    const dialog = wrapper.get('[role="dialog"]')
    expect(dialog.attributes('aria-modal')).toBe('true')
    const labelId = dialog.attributes('aria-labelledby') ?? ''
    expect(document.getElementById(labelId)?.textContent?.trim()).toBe('Example')
    expect(document.activeElement?.id).toBe('first')
  })

  it('keeps Tab and Shift+Tab inside the dialog', async () => {
    const wrapper = await openModal()
    ;(document.getElementById('last') as HTMLElement).focus()
    await wrapper.get('[role="dialog"]').trigger('keydown', { key: 'Tab' })
    expect(document.activeElement?.id).toBe('first')

    await wrapper.get('[role="dialog"]').trigger('keydown', { key: 'Tab', shiftKey: true })
    expect(document.activeElement?.id).toBe('last')
  })

  it('closes on Escape and gives focus back to the button that opened it', async () => {
    const wrapper = await openModal()
    await wrapper.get('[role="dialog"]').trigger('keydown', { key: 'Escape' })
    await nextTick()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(document.activeElement?.id).toBe('opener')
  })
})
