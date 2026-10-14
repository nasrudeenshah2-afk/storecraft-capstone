import { defineStore } from 'pinia'
import { ref } from 'vue'

export type ToastKind = 'success' | 'error' | 'info'

export interface Toast {
  id: number
  kind: ToastKind
  message: string
}

/** How long a toast stays before it dismisses itself. */
export const TOAST_TIMEOUT_MS = 5000

/** Shared toast list (rendered once by ToastHost). */
export const useToastStore = defineStore('toasts', () => {
  const toasts = ref<Toast[]>([])
  let nextId = 1

  function dismiss(id: number): void {
    toasts.value = toasts.value.filter((t) => t.id !== id)
  }

  function show(kind: ToastKind, message: string): number {
    const id = nextId++
    toasts.value = [...toasts.value, { id, kind, message }]
    setTimeout(() => dismiss(id), TOAST_TIMEOUT_MS)
    return id
  }

  return { toasts, show, dismiss }
})

/** Show short messages from any component: useToast().success('Saved'). */
export function useToast() {
  const store = useToastStore()
  return {
    success: (message: string) => store.show('success', message),
    error: (message: string) => store.show('error', message),
    info: (message: string) => store.show('info', message),
    dismiss: (id: number) => store.dismiss(id),
  }
}
