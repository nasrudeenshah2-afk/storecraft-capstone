<script setup lang="ts">
import { computed, watch } from 'vue'
import type { Usage } from '../api/client'
import type { UsageStatus } from '../composables/useUsage'
import { track } from '../events'
import { bytesText, keysText, meterState, type MeterState } from '../quota'
import BaseButton from './BaseButton.vue'

const props = defineProps<{
  usage: Usage | null
  status: UsageStatus
  error: string
}>()

const emit = defineEmits<{
  retry: []
}>()

interface Row {
  dimension: 'keys' | 'bytes'
  label: string
  text: string
  used: number
  limit: number | null
  state: MeterState
}

const rows = computed<Row[]>(() => {
  const u = props.usage
  if (u === null) return []
  return [
    { dimension: 'keys', label: 'Keys used', text: keysText(u.keys.used, u.keys.limit), used: u.keys.used, limit: u.keys.limit, state: meterState(u.keys.used, u.keys.limit) },
    { dimension: 'bytes', label: 'Bytes used', text: bytesText(u.bytes.used, u.bytes.limit), used: u.bytes.used, limit: u.bytes.limit, state: meterState(u.bytes.used, u.bytes.limit) },
  ]
})

const stateText: Record<MeterState, string> = {
  unlimited: '',
  normal: '',
  near: 'Almost full',
  over: 'Over quota',
}

function fill(row: Row): string {
  if (row.limit === null || row.limit === 0) return row.used > 0 ? '100%' : '0%'
  return `${Math.min(100, (row.used / row.limit) * 100)}%`
}

// Client event: a dimension has just entered the near-limit state.
watch(
  () => rows.value.map((r) => `${r.dimension}:${r.state}`),
  (now, before) => {
    for (const r of rows.value) {
      if (r.state === 'near' && !(before ?? []).includes(`${r.dimension}:near`)) {
        track('usage_near_limit_shown', { dimension: r.dimension })
      }
    }
    void now
  },
  { immediate: true },
)
</script>

<template>
  <section
    class="usage"
    aria-labelledby="usage-heading"
  >
    <h2 id="usage-heading">
      Storage usage
    </h2>
    <p
      v-if="status === 'loading'"
      role="status"
    >
      Loading usage…
    </p>
    <div
      v-else-if="status === 'error'"
      role="alert"
      class="usage-error"
    >
      <p>Could not load usage. {{ error }}</p>
      <BaseButton @click="emit('retry')">
        Try again
      </BaseButton>
    </div>
    <ul
      v-else
      class="rows"
      aria-live="polite"
    >
      <li
        v-for="row in rows"
        :key="row.dimension"
        :class="`row state-${row.state}`"
      >
        <span class="text">{{ row.text }}<strong
          v-if="stateText[row.state] !== ''"
          class="badge"
        > – {{ stateText[row.state] }}</strong></span>
        <div
          v-if="row.limit !== null"
          class="meter"
          role="meter"
          :aria-label="row.label"
          aria-valuemin="0"
          :aria-valuemax="row.limit"
          :aria-valuenow="row.used"
          :aria-valuetext="row.text"
        >
          <span
            class="fill"
            :style="{ width: fill(row) }"
          />
        </div>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.usage {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 0.75rem 1rem;
  margin-bottom: 1rem;
}
h2 {
  font-size: 1rem;
  margin: 0 0 0.5rem;
}
.rows {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 0.6rem;
}
.text {
  display: block;
  margin-bottom: 0.25rem;
}
.meter {
  height: 0.6rem;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 999px;
  overflow: hidden;
}
.fill {
  display: block;
  height: 100%;
  background: var(--accent);
}
.state-near .fill {
  background: #8a5a00;
}
.state-over .fill {
  background: var(--danger);
}
.badge {
  font-weight: 700;
}
.usage-error p {
  margin-top: 0;
}
</style>
