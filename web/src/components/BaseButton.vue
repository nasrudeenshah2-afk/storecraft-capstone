<script setup lang="ts">
withDefaults(
  defineProps<{
    variant?: 'primary' | 'secondary' | 'danger'
    type?: 'button' | 'submit'
    disabled?: boolean
    /** Shows the button as working and blocks repeat clicks. */
    busy?: boolean
  }>(),
  { variant: 'secondary', type: 'button', disabled: false, busy: false },
)
</script>

<template>
  <button
    :type="type"
    :class="['btn', `btn-${variant}`]"
    :disabled="disabled || busy"
    :aria-busy="busy ? 'true' : undefined"
  >
    <slot />
  </button>
</template>

<style scoped>
.btn {
  font: inherit;
  padding: 0.45rem 0.9rem;
  border-radius: var(--radius);
  border: 1px solid var(--border-strong);
  background: var(--surface);
  color: var(--text);
  cursor: pointer;
}
.btn:hover:not(:disabled) {
  background: var(--surface-hover);
}
.btn:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}
.btn-primary {
  background: var(--accent);
  border-color: var(--accent);
  color: var(--on-accent);
}
.btn-primary:hover:not(:disabled) {
  background: var(--accent-hover);
}
.btn-danger {
  background: var(--danger);
  border-color: var(--danger);
  color: var(--on-accent);
}
.btn-danger:hover:not(:disabled) {
  background: var(--danger-hover);
}
</style>
