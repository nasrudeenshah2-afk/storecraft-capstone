<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'

const props = defineProps<{
  open: boolean
  title: string
}>()

const emit = defineEmits<{
  close: []
}>()

const titleId = useId()
const dialog = ref<HTMLElement | null>(null)
let returnFocusTo: HTMLElement | null = null

const focusableSelector =
  'a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'

function focusables(): HTMLElement[] {
  return dialog.value === null ? [] : Array.from(dialog.value.querySelectorAll<HTMLElement>(focusableSelector))
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') {
    event.preventDefault()
    emit('close')
    return
  }
  if (event.key !== 'Tab') {
    return
  }
  // Keep keyboard focus inside the dialog.
  const items = focusables()
  const first = items[0]
  const last = items[items.length - 1]
  if (first === undefined || last === undefined) {
    event.preventDefault()
    return
  }
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault()
    first.focus()
  }
}

watch(
  () => props.open,
  async (open) => {
    if (open) {
      returnFocusTo = document.activeElement instanceof HTMLElement ? document.activeElement : null
      await nextTick()
      const target = focusables()[0] ?? dialog.value
      target?.focus()
    } else {
      // Give focus back to whatever opened the dialog.
      returnFocusTo?.focus()
      returnFocusTo = null
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  returnFocusTo?.focus()
})
</script>

<template>
  <div
    v-if="open"
    class="backdrop"
    @mousedown.self="emit('close')"
  >
    <div
      ref="dialog"
      class="dialog"
      role="dialog"
      aria-modal="true"
      :aria-labelledby="titleId"
      tabindex="-1"
      @keydown="onKeydown"
    >
      <h2
        :id="titleId"
        class="title"
      >
        {{ title }}
      </h2>
      <slot />
      <div class="actions">
        <slot name="actions" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.backdrop {
  position: fixed;
  inset: 0;
  background: rgb(0 0 0 / 0.45);
  display: grid;
  place-items: center;
  padding: 1rem;
  z-index: 10;
}
.dialog {
  background: var(--surface);
  color: var(--text);
  border-radius: var(--radius);
  padding: 1.25rem;
  width: min(36rem, 100%);
  max-height: 90vh;
  overflow: auto;
  box-shadow: 0 10px 30px rgb(0 0 0 / 0.3);
}
.title {
  margin: 0 0 1rem;
  font-size: 1.2rem;
}
.actions {
  display: flex;
  justify-content: flex-end;
  gap: 0.5rem;
  margin-top: 1rem;
}
</style>
