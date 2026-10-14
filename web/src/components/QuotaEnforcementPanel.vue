<script setup lang="ts">
import { computed, ref } from 'vue'
import { ApiError, describeError } from '../api/errors'
import { useApi } from '../api/useApi'
import { track } from '../events'
import { useFlagsStore } from '../stores/flags'
import BaseButton from './BaseButton.vue'
import BaseModal from './BaseModal.vue'

// Admin switch for the quota_enforcement flag (AC-UI-7). It always stays
// visible, also while the flag is off, so the feature can be turned on.

const api = useApi()
const flags = useFlagsStore()
const current = computed(() => flags.quotaEnforcement)

const confirmOpen = ref(false)
/** The state the admin is asked to confirm, and the version they saw when asking. */
const target = ref(false)
const seenVersion = ref(0)
const busy = ref(false)
const stale = ref(false)
const error = ref('')
const announcement = ref('')

function ask(): void {
  const f = current.value
  if (f === null) return
  target.value = !f.enabled
  seenVersion.value = f.version
  stale.value = false
  error.value = ''
  confirmOpen.value = true
}

async function confirm(): Promise<void> {
  busy.value = true
  try {
    // If-Match: the version shown when the admin asked, so a change made by
    // someone else in the meantime is a 412, never silently overwritten.
    const res = await api.setQuotaEnforcement(target.value, seenVersion.value)
    flags.set(res.data)
    track('quota_flag_toggled', { enabled: res.data.enabled })
    announcement.value = `Quota enforcement turned ${res.data.enabled ? 'on' : 'off'}.`
  } catch (err) {
    if (err instanceof ApiError && err.code === 'precondition_failed') {
      stale.value = true
    } else {
      error.value = describeError(err)
    }
  } finally {
    busy.value = false
    confirmOpen.value = false
  }
}

async function reload(): Promise<void> {
  stale.value = false
  await flags.refresh()
}
</script>

<template>
  <section
    class="panel"
    aria-labelledby="flag-heading"
  >
    <h2 id="flag-heading">
      Quota enforcement
    </h2>
    <div
      v-if="current === null && flags.error !== ''"
      class="alert"
      role="alert"
    >
      <p>Could not read the current setting. {{ flags.error }}</p>
      <BaseButton @click="flags.refresh()">
        Try again
      </BaseButton>
    </div>
    <p v-else-if="current === null">
      Loading the current setting…
    </p>
    <template v-else>
      <p>
        <strong>Quota enforcement is {{ current.enabled ? 'on' : 'off' }}.</strong>
        {{ current.enabled
          ? 'Writes that would go over a tenant\'s quota are rejected. Usage and quota tools are shown.'
          : 'No write is rejected for quota; usage is still counted. Usage and quota tools are hidden.' }}
      </p>
      <BaseButton
        :variant="current.enabled ? 'danger' : 'primary'"
        @click="ask"
      >
        {{ current.enabled ? 'Turn off' : 'Turn on' }}
      </BaseButton>
    </template>
    <p
      class="visually-hidden"
      role="status"
    >
      {{ announcement }}
    </p>
    <div
      v-if="stale"
      class="alert"
      role="alert"
    >
      <p>Someone else changed this setting. Reload to see the current state.</p>
      <BaseButton @click="reload">
        Reload
      </BaseButton>
    </div>
    <p
      v-if="error !== ''"
      class="alert"
      role="alert"
    >
      {{ error }}
    </p>

    <BaseModal
      :open="confirmOpen"
      :title="target ? 'Turn on quota enforcement?' : 'Turn off quota enforcement?'"
      @close="confirmOpen = false"
    >
      <p v-if="target">
        Writes over quota will be rejected. Tenants at or over a limit can still delete keys and make values smaller.
      </p>
      <p v-else>
        Writes over quota will be accepted again. Usage is still counted, and the usage and quota tools are hidden.
      </p>
      <template #actions>
        <BaseButton @click="confirmOpen = false">
          Cancel
        </BaseButton>
        <BaseButton
          :variant="target ? 'primary' : 'danger'"
          :busy="busy"
          @click="confirm"
        >
          {{ target ? 'Turn on' : 'Turn off' }}
        </BaseButton>
      </template>
    </BaseModal>
  </section>
</template>

<style scoped>
.panel {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 1rem 1.5rem;
  margin-top: 1rem;
}
h2 {
  margin-top: 0;
  font-size: 1.15rem;
}
.alert {
  color: var(--danger);
  margin-top: 0.75rem;
}
</style>
