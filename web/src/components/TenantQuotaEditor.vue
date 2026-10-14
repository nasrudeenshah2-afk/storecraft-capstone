<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import type { Quota, QuotaUpdate, TenantSummary } from '../api/client'
import { ApiError, describeError } from '../api/errors'
import { useApi } from '../api/useApi'
import { track } from '../events'
import { LIMIT_FIELD_ERROR, bytesText, formatBytes, formatCount, parseLimitField } from '../quota'
import BaseButton from './BaseButton.vue'
import BaseTextField from './BaseTextField.vue'
import ConflictDialog from './ConflictDialog.vue'

// Admin quota editor (AC-UI-4, AC-UI-5): choose a tenant, see its quota and
// usage, edit the limits; saves send If-Match with the version shown, and a
// 412 opens the conflict dialog.

const api = useApi()

const tenants = ref<TenantSummary[]>([])
const listError = ref('')
const selected = ref('') // tenant id as text ('' = none chosen)
const tenant = computed(() => tenants.value.find((t) => String(t.id) === selected.value) ?? null)

/** The quota as last read from the server (its version is sent with Save). */
const quota = ref<Quota | null>(null)
const maxKeys = ref('')
const maxBytes = ref('')
const keysError = ref('')
const bytesError = ref('')
const saving = ref(false)
const saveError = ref('')
const status = ref('')

const conflictOpen = ref(false)
const conflictLatest = ref<Quota | null>(null)
const conflictBusy = ref(false)
const conflictError = ref('')
const SAVE_ID = 'quota-save'

async function loadTenants(): Promise<void> {
  try {
    tenants.value = (await api.listTenants()).data.tenants
    listError.value = ''
  } catch (err) {
    listError.value = describeError(err)
  }
}

function fill(q: Quota): void {
  quota.value = q
  maxKeys.value = q.max_keys === null ? '' : String(q.max_keys)
  maxBytes.value = q.max_bytes === null ? '' : String(q.max_bytes)
  keysError.value = bytesError.value = ''
}

watch(selected, async (id) => {
  quota.value = null
  status.value = saveError.value = ''
  if (id === '') return
  try {
    const q = (await api.getQuota(Number(id))).data
    if (selected.value === id) fill(q) // ignore a late answer for a tenant no longer chosen (review R1)
  } catch (err) {
    if (selected.value === id) saveError.value = describeError(err)
  }
})

/** The limits from the form, or null (with field errors shown) if invalid. */
function readForm(): QuotaUpdate | null {
  const k = parseLimitField(maxKeys.value)
  const b = parseLimitField(maxBytes.value)
  keysError.value = k === undefined ? LIMIT_FIELD_ERROR : ''
  bytesError.value = b === undefined ? LIMIT_FIELD_ERROR : ''
  if (k === undefined || b === undefined) return null
  return { max_keys: k, max_bytes: b }
}

/** The form's values for the conflict dialog. Pure: it shows no field errors (review R7). */
function yourValues(): string {
  const show = (text: string) => {
    const v = parseLimitField(text)
    return v === undefined ? text : v
  }
  return JSON.stringify({ max_keys: show(maxKeys.value), max_bytes: show(maxBytes.value) }, null, 2)
}

/** Saves the form on top of the given version. Returns false on a 412. */
async function put(version: number): Promise<boolean> {
  const t = tenant.value
  const update = readForm()
  if (t === null || update === null) return true
  const res = await api.putQuota(t.id, update, version)
  if (tenant.value?.id === t.id) fill(res.data) // the admin may have chosen another tenant meanwhile (review R1)
  status.value = `Quota saved (version ${res.data.version}).`
  track('quota_editor_saved', { tenant_id: t.id, version: res.data.version })
  void loadTenants() // refresh the usage shown
  return true
}

async function openConflict(): Promise<void> {
  const t = tenant.value
  if (t === null) return
  track('quota_editor_conflict', { tenant_id: t.id })
  conflictLatest.value = (await api.getQuota(t.id)).data
  conflictError.value = ''
  conflictOpen.value = true
}

async function save(): Promise<void> {
  const q = quota.value
  if (q === null) return
  status.value = saveError.value = ''
  saving.value = true
  try {
    await put(q.version)
  } catch (err) {
    if (err instanceof ApiError && err.code === 'precondition_failed') {
      await openConflict().catch((e: unknown) => (saveError.value = describeError(e)))
    } else {
      saveError.value = describeError(err)
    }
  } finally {
    saving.value = false
  }
}

/** Conflict: save my values on top of the latest version. */
async function overwrite(): Promise<void> {
  const latest = conflictLatest.value
  if (latest === null) return
  conflictBusy.value = true
  try {
    await put(latest.version)
    void closeConflict()
  } catch (err) {
    if (err instanceof ApiError && err.code === 'precondition_failed' && tenant.value !== null) {
      try {
        conflictLatest.value = (await api.getQuota(tenant.value.id)).data
        conflictError.value = 'It changed again while you were deciding. These are the newest values.'
      } catch (e) {
        conflictError.value = `It changed again, and the newest values could not be read: ${describeError(e)}` // review R4
      }
    } else {
      conflictError.value = describeError(err)
    }
  } finally {
    conflictBusy.value = false
  }
}

/**
 * Closes the conflict dialog and puts keyboard focus back on Save (the dialog
 * cannot do that itself: Save was disabled while saving, so focus had left it).
 */
async function closeConflict(): Promise<void> {
  conflictOpen.value = false
  await nextTick()
  document.getElementById(SAVE_ID)?.focus()
}

/** Conflict: load the latest values into the editor. */
function reloadLatest(): void {
  if (conflictLatest.value !== null) fill(conflictLatest.value)
  status.value = 'Loaded the latest values.'
  void closeConflict()
}

onMounted(() => void loadTenants())
</script>

<template>
  <section
    class="panel"
    aria-labelledby="quotas-heading"
  >
    <h2 id="quotas-heading">
      Tenant quotas
    </h2>
    <p
      v-if="listError !== ''"
      role="alert"
      class="alert"
    >
      {{ listError }}
    </p>
    <div class="field">
      <label for="quota-tenant">Tenant</label>
      <select
        id="quota-tenant"
        v-model="selected"
      >
        <option value="">
          Choose a tenant…
        </option>
        <option
          v-for="t in tenants"
          :key="t.id"
          :value="String(t.id)"
        >
          {{ t.name }}
        </option>
      </select>
    </div>

    <form
      v-if="tenant !== null && quota !== null"
      class="editor"
      novalidate
      @submit.prevent="save"
    >
      <p class="facts">
        Usage: {{ formatCount(tenant.usage.keys) }} {{ tenant.usage.keys === 1 ? 'key' : 'keys' }}, {{ formatBytes(tenant.usage.bytes) }}.
        Version {{ quota.version }}{{ quota.version === 0 ? ' (default quota, never edited)' : '' }}.
      </p>
      <BaseTextField
        v-model="maxKeys"
        label="Max keys"
        hint="Leave empty for unlimited."
        :error="keysError"
      />
      <BaseTextField
        v-model="maxBytes"
        label="Max bytes"
        :hint="`Leave empty for unlimited. Currently ${maxBytes === '' ? 'unlimited' : bytesText(0, Number(maxBytes)).replace('0 bytes of ', '')}.`"
        :error="bytesError"
      />
      <BaseButton
        :id="SAVE_ID"
        type="submit"
        variant="primary"
        :busy="saving"
      >
        Save quota
      </BaseButton>
      <p
        v-if="saveError !== ''"
        role="alert"
        class="alert"
      >
        {{ saveError }}
      </p>
    </form>
    <p
      class="status"
      aria-live="polite"
    >
      {{ status }}
    </p>

    <ConflictDialog
      :open="conflictOpen"
      :key-name="tenant?.name ?? ''"
      :title="`Conflict: the quota of &quot;${tenant?.name ?? ''}&quot; changed`"
      noun="values"
      intro="Another admin saved this quota while you were editing. Compare the two and choose what to keep."
      :your-value="yourValues()"
      :latest="conflictLatest === null ? null : { value: { max_keys: conflictLatest.max_keys, max_bytes: conflictLatest.max_bytes }, version: conflictLatest.version }"
      :busy="conflictBusy"
      :error="conflictError"
      @overwrite="overwrite"
      @reload="reloadLatest"
      @cancel="closeConflict"
    />
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
.field {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  max-width: 20rem;
  margin-bottom: 0.75rem;
}
select {
  font: inherit;
  padding: 0.4rem;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
}
.editor {
  max-width: 28rem;
}
.facts {
  color: var(--text-muted);
}
.alert {
  color: var(--danger);
}
.status {
  min-height: 1.2em;
  margin-bottom: 0;
}
</style>
