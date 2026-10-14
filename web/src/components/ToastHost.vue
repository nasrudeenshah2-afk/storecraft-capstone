<script setup lang="ts">
import { computed } from 'vue'
import { useToastStore } from '../composables/useToast'

const store = useToastStore()
const errors = computed(() => store.toasts.filter((t) => t.kind === 'error'))
const others = computed(() => store.toasts.filter((t) => t.kind !== 'error'))
</script>

<template>
  <div class="toast-host">
    <!-- Errors are announced immediately; other messages politely. -->
    <div
      role="alert"
      aria-live="assertive"
    >
      <div
        v-for="t in errors"
        :key="t.id"
        class="toast toast-error"
      >
        <span>{{ t.message }}</span>
        <button
          type="button"
          class="dismiss"
          aria-label="Dismiss message"
          @click="store.dismiss(t.id)"
        >
          ×
        </button>
      </div>
    </div>
    <div
      role="status"
      aria-live="polite"
    >
      <div
        v-for="t in others"
        :key="t.id"
        :class="['toast', `toast-${t.kind}`]"
      >
        <span>{{ t.message }}</span>
        <button
          type="button"
          class="dismiss"
          aria-label="Dismiss message"
          @click="store.dismiss(t.id)"
        >
          ×
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.toast-host {
  position: fixed;
  right: 1rem;
  bottom: 1rem;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  z-index: 20;
  max-width: min(26rem, calc(100vw - 2rem));
}
.toast {
  display: flex;
  align-items: start;
  gap: 0.75rem;
  justify-content: space-between;
  padding: 0.6rem 0.8rem;
  margin-top: 0.5rem;
  border-radius: var(--radius);
  color: var(--on-accent);
  box-shadow: 0 4px 14px rgb(0 0 0 / 0.25);
}
.toast-success {
  background: var(--success);
}
.toast-info {
  background: var(--accent);
}
.toast-error {
  background: var(--danger);
}
.dismiss {
  background: none;
  border: none;
  color: inherit;
  font-size: 1.2rem;
  line-height: 1;
  cursor: pointer;
}
</style>
