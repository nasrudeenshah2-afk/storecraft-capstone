<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import type { ReplicationStatus } from '../api/client'
import { describeError } from '../api/errors'
import { useApi } from '../api/useApi'

const props = withDefaults(defineProps<{ intervalMs?: number }>(), { intervalMs: 5000 })

const api = useApi()
const status = ref<ReplicationStatus | null>(null)
const error = ref('')
let timer: ReturnType<typeof setTimeout> | undefined
let stopped = false

async function refresh(): Promise<void> {
  try {
    status.value = (await api.replicationStatus()).data
    error.value = ''
  } catch (err) {
    error.value = describeError(err)
  }
  if (!stopped) {
    timer = setTimeout(() => void refresh(), props.intervalMs)
  }
}

onMounted(() => {
  void refresh()
})

onBeforeUnmount(() => {
  stopped = true
  clearTimeout(timer)
})
</script>

<template>
  <div
    class="badge"
    :class="{ lagging: status !== null && status.lag > 0, failed: error !== '' }"
    aria-label="Replication status"
  >
    <template v-if="error !== ''">
      <span class="label">Replication:</span> unavailable ({{ error }})
    </template>
    <template v-else-if="status === null">
      <span class="label">Replication:</span> loading…
    </template>
    <template v-else>
      <span class="label">{{ status.role === 'primary' ? 'Primary' : 'Replica' }}</span>
      <span>applied {{ status.applied_seq }}</span>
      <span>primary {{ status.primary_seq }}</span>
      <span>lag {{ status.lag }}{{ status.lag > 0 ? ' (behind)' : ' (in sync)' }}</span>
    </template>
  </div>
</template>

<style scoped>
.badge {
  display: inline-flex;
  flex-wrap: wrap;
  gap: 0.6rem;
  align-items: center;
  font-size: 0.9em;
  padding: 0.25rem 0.6rem;
  border-radius: 999px;
  border: 1px solid var(--success);
  color: var(--text);
  background: var(--surface);
}
.badge.lagging {
  border-color: var(--focus);
}
.badge.failed {
  border-color: var(--danger);
}
.label {
  font-weight: 700;
}
</style>
