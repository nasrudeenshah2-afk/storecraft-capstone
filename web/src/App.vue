<script setup lang="ts">
import BaseButton from './components/BaseButton.vue'
import ReplicationBadge from './components/ReplicationBadge.vue'
import ToastHost from './components/ToastHost.vue'
import { watch } from 'vue'
import { useApi } from './api/useApi'
import { useToast } from './composables/useToast'
import { useFlagsStore } from './stores/flags'
import { useSessionStore } from './stores/session'
import AdminHomeView from './views/AdminHomeView.vue'
import KeysView from './views/KeysView.vue'
import LoginView from './views/LoginView.vue'

const session = useSessionStore()
const toast = useToast()
const flags = useFlagsStore()
const api = useApi()

// Runtime flags are polled while signed in, for every open console (AC-UI-6).
watch(
  () => session.isLoggedIn,
  (loggedIn) => (loggedIn ? flags.start(api) : flags.stop()),
  { immediate: true },
)

function signOut(): void {
  session.logout()
  toast.info('Signed out.')
}
</script>

<template>
  <header class="app-header">
    <span class="brand">storecraft console</span>
    <div
      v-if="session.isLoggedIn"
      class="who"
    >
      <ReplicationBadge v-if="session.role === 'admin'" />
      <span>Signed in as <strong>{{ session.role === 'admin' ? 'Admin' : 'Tenant' }}</strong></span>
      <BaseButton @click="signOut">
        Sign out
      </BaseButton>
    </div>
  </header>
  <main class="app-main">
    <LoginView v-if="!session.isLoggedIn" />
    <KeysView v-else-if="session.role === 'tenant'" />
    <AdminHomeView v-else />
  </main>
  <ToastHost />
</template>

<style scoped>
.app-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
  padding: 0.75rem 1.25rem;
  background: var(--surface);
  border-bottom: 1px solid var(--border);
}
.brand {
  font-weight: 700;
}
.who {
  display: flex;
  align-items: center;
  gap: 0.75rem;
}
.app-main {
  max-width: 64rem;
  margin: 0 auto;
  padding: 1.25rem;
}
</style>
