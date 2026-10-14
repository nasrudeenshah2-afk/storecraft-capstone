<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import type { Entry } from '../api/client'
import { useApi } from '../api/useApi'
import BaseButton from '../components/BaseButton.vue'
import BaseModal from '../components/BaseModal.vue'
import BaseTextField from '../components/BaseTextField.vue'
import ConflictDialog from '../components/ConflictDialog.vue'
import UsageMeter from '../components/UsageMeter.vue'
import { useChangeFeed, type FeedStatus } from '../composables/useChangeFeed'
import { useToast } from '../composables/useToast'
import { useUsage } from '../composables/useUsage'
import { track } from '../events'
import { OVER_QUOTA_ADVICE, overQuotaMessage } from '../quota'
import { useFlagsStore } from '../stores/flags'
import { useSessionStore } from '../stores/session'
import { editableValue, previewValue, useKeys, type Result } from './useKeys'

const session = useSessionStore()
const toast = useToast()
const api = useApi()

let signedOut = false
function tokenRejected(): void {
  if (signedOut) return
  signedOut = true
  session.logout()
  toast.error('Your token is no longer accepted. Sign in again.')
}

const keys = useKeys(api, tokenRejected)
const { items, prefix, appliedPrefix, loading, loadError } = keys

// STORE-210: the usage meter, shown only while quota_enforcement is on (the
// flag is read from the flags store, the one place it is read from).
const flags = useFlagsStore()
const usage = useUsage(api, tokenRejected)
watch(
  () => flags.quotaEnabled,
  (on) => (on ? usage.start() : usage.stop()),
  { immediate: true },
)

// Live updates: follow the change feed from the seq of the first list. Every
// change also refreshes the usage meter (debounced).
const feed = useChangeFeed({
  api,
  onChange: (change) => {
    keys.applyChange(change)
    if (flags.quotaEnabled) usage.changed()
  },
  onUnauthorized: tokenRejected,
})
const feedLabels: Record<FeedStatus, string> = {
  idle: 'Not connected',
  connecting: 'Connecting…',
  live: 'Live',
  reconnecting: 'Reconnecting…',
  fallback: 'Live (polling)',
  stopped: 'Disconnected',
}
const feedLabel = computed(() => feedLabels[feed.status.value])

async function loadList(): Promise<void> {
  await keys.load()
  if (keys.loadError.value === '' && feed.status.value === 'idle') {
    feed.start(keys.listSeq.value)
  }
}

// Editor (create or edit).
const editorOpen = ref(false)
const editing = ref<Entry | null>(null) // null = creating a new key
const formKey = ref('')
const formValue = ref('')
const keyError = ref('')
const valueError = ref('')
const formError = ref('')
const saving = ref(false)
const editorTitle = computed(() => (editing.value === null ? 'New key' : `Edit "${editing.value.key}"`))

/** What the live list currently shows for the key being edited. */
const liveNotice = computed(() => {
  const e = editing.value
  if (e === null || !editorOpen.value) return ''
  const now = items.value.find((i) => i.key === e.key)
  if (now === undefined) return `Someone deleted "${e.key}" while you were editing. Saving will ask what to do.`
  if (now.version !== e.version) {
    return `Someone changed "${e.key}" while you were editing (it is now version ${now.version}). Saving will ask what to do.`
  }
  return ''
})

// Conflict dialog (a save that hit 412).
const conflictOpen = ref(false)
const conflictLatest = ref<Entry | null>(null)
const conflictBusy = ref(false)
const conflictError = ref('')

// Delete confirmation.
const deleting = ref<Entry | null>(null)
const deleteError = ref('')
const deleteBusy = ref(false)

function openCreate(): void {
  editing.value = null
  formKey.value = ''
  formValue.value = ''
  keyError.value = valueError.value = formError.value = ''
  editorOpen.value = true
}

function openEdit(entry: Entry): void {
  editing.value = entry
  formKey.value = entry.key
  formValue.value = editableValue(entry.value)
  keyError.value = valueError.value = formError.value = ''
  editorOpen.value = true
}

function closeEditor(): void {
  editorOpen.value = false
}

/** Fetches the key's current state and opens the conflict dialog. */
async function openConflict(key: string): Promise<void> {
  const res = await keys.latest(key)
  if (!res.ok) {
    formError.value = res.message
    return
  }
  conflictLatest.value = res.entry
  conflictError.value = ''
  conflictOpen.value = true
}

async function submitEditor(): Promise<void> {
  keyError.value = valueError.value = formError.value = ''
  saving.value = true
  const entry = editing.value
  const result = entry === null ? await keys.create(formKey.value, formValue.value) : await keys.save(entry, formValue.value)
  saving.value = false
  if (result.ok) {
    editorOpen.value = false
    toast.success(entry === null ? `Created "${formKey.value}".` : `Saved "${formKey.value}".`)
    return
  }
  // Keep the modal open with the user's input intact.
  if (result.code === 'validation' && 'field' in result) {
    if (result.field === 'key') {
      keyError.value = result.message
    } else {
      valueError.value = result.message
    }
  } else if (result.code === 'precondition_failed' && entry !== null) {
    await openConflict(entry.key)
  } else {
    formError.value = failureText(result)
  }
}

/**
 * The text for a failed save, chosen by the error code. quota_exceeded uses
 * the numbers from the error details (never the server's message text).
 */
function failureText(result: Extract<Result, { ok: false }>): string {
  if (result.code !== 'quota_exceeded') {
    return result.message
  }
  const details = ('details' in result ? result.details : null) ?? null
  track('quota_rejected_shown', { dimension: details?.dimension ?? 'unknown' })
  return details !== null ? overQuotaMessage(details) : `${result.message} ${OVER_QUOTA_ADVICE}`
}

/** Conflict: save my text on top of the latest version (or re-create it). */
async function conflictOverwrite(): Promise<void> {
  const entry = editing.value
  if (entry === null) return
  conflictBusy.value = true
  conflictError.value = ''
  const latest = conflictLatest.value
  const result = latest === null ? await keys.create(entry.key, formValue.value) : await keys.save(latest, formValue.value)
  conflictBusy.value = false
  if (result.ok) {
    conflictOpen.value = false
    editorOpen.value = false
    toast.success(`Saved "${entry.key}".`)
  } else if (result.code === 'precondition_failed') {
    // It changed yet again: show the newest value and let the user decide again.
    const res = await keys.latest(entry.key)
    if (res.ok) {
      conflictLatest.value = res.entry
      conflictError.value = 'It changed again while you were deciding. This is the newest value.'
    } else {
      conflictError.value = res.message
    }
  } else {
    conflictError.value = failureText(result)
  }
}

/** Conflict: discard my text and continue editing from the latest value. */
function conflictReload(): void {
  const latest = conflictLatest.value
  conflictOpen.value = false
  if (latest === null) {
    editorOpen.value = false
    toast.info(`"${formKey.value}" was deleted; your edit was discarded.`)
    return
  }
  editing.value = latest
  formValue.value = editableValue(latest.value)
}

function openDelete(entry: Entry): void {
  deleting.value = entry
  deleteError.value = ''
}

async function confirmDelete(): Promise<void> {
  const entry = deleting.value
  if (entry === null) return
  deleteBusy.value = true
  const result = await keys.remove(entry.key)
  deleteBusy.value = false
  if (result.ok) {
    deleting.value = null
    toast.success(`Deleted "${entry.key}".`)
  } else {
    deleteError.value = result.message
  }
}

onMounted(() => {
  void loadList()
})</script>

<template>
  <section aria-labelledby="keys-heading">
    <div class="toolbar">
      <h1 id="keys-heading">
        Keys
      </h1>
      <span
        class="feed"
        :class="`feed-${feed.status.value}`"
        role="status"
        aria-live="polite"
        data-testid="feed-status"
      ><span
        class="dot"
        aria-hidden="true"
      />{{ feedLabel }}</span>
      <span class="spacer" />
      <BaseButton
        variant="primary"
        @click="openCreate"
      >
        New key
      </BaseButton>
    </div>

    <UsageMeter
      v-if="flags.quotaEnabled"
      :usage="usage.usage.value"
      :status="usage.status.value"
      :error="usage.error.value"
      @retry="usage.load()"
    />

    <form
      class="filter"
      role="search"
      @submit.prevent="loadList()"
    >
      <BaseTextField
        v-model="prefix"
        label="Filter by prefix"
        hint="Shows keys that start with this text."
      />
      <div class="filter-actions">
        <BaseButton
          type="submit"
          :busy="loading"
        >
          Filter
        </BaseButton>
        <BaseButton
          v-if="appliedPrefix !== ''"
          @click="keys.clearFilter()"
        >
          Clear filter
        </BaseButton>
      </div>
    </form>

    <p
      class="status"
      role="status"
      aria-live="polite"
    >
      <template v-if="loading">
        Loading keys…
      </template>
      <template v-else-if="loadError === ''">
        {{ items.length }} key{{ items.length === 1 ? '' : 's' }}<template v-if="appliedPrefix !== ''">
          starting with "{{ appliedPrefix }}"
        </template>
      </template>
    </p>

    <div
      v-if="loadError !== ''"
      class="load-error"
      role="alert"
    >
      <p>{{ loadError }}</p>
      <BaseButton @click="loadList()">
        Try again
      </BaseButton>
    </div>

    <table
      v-else-if="items.length > 0"
      class="keys"
    >
      <caption class="visually-hidden">
        Keys and their current values
      </caption>
      <thead>
        <tr>
          <th scope="col">
            Key
          </th>
          <th scope="col">
            Value
          </th>
          <th scope="col">
            Version
          </th>
          <th scope="col">
            <span class="visually-hidden">Actions</span>
          </th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="entry in items"
          :key="entry.key"
        >
          <th
            scope="row"
            class="mono"
          >
            {{ entry.key }}
          </th>
          <td class="mono value">
            {{ previewValue(entry.value) }}
          </td>
          <td>{{ entry.version }}</td>
          <td class="row-actions">
            <BaseButton
              :aria-label="`Edit ${entry.key}`"
              @click="openEdit(entry)"
            >
              Edit
            </BaseButton>
            <BaseButton
              variant="danger"
              :aria-label="`Delete ${entry.key}`"
              @click="openDelete(entry)"
            >
              Delete
            </BaseButton>
          </td>
        </tr>
      </tbody>
    </table>

    <p
      v-else-if="!loading"
      class="empty"
    >
      No keys yet. Use "New key" to create one.
    </p>

    <BaseModal
      :open="editorOpen"
      :title="editorTitle"
      @close="closeEditor"
    >
      <form
        id="key-editor"
        @submit.prevent="submitEditor"
      >
        <BaseTextField
          v-if="editing === null"
          v-model="formKey"
          label="Key"
          required
          monospace
          :error="keyError"
          hint="Letters, digits and other characters; '/' groups keys, for example app/theme."
        />
        <p
          v-else
          class="editing-note"
        >
          Editing version {{ editing.version }}. The save only succeeds if nobody changed the key since.
        </p>
        <p
          v-if="liveNotice !== ''"
          class="live-notice"
          role="status"
        >
          {{ liveNotice }}
        </p>
        <BaseTextField
          v-model="formValue"
          label="Value (JSON)"
          multiline
          :rows="8"
          required
          monospace
          :error="valueError"
          hint="Any JSON value, for example &quot;hello&quot;, 42, true or {&quot;theme&quot;: &quot;dark&quot;}."
        />
        <p
          v-if="formError !== ''"
          class="form-error"
          role="alert"
        >
          {{ formError }}
        </p>
      </form>
      <template #actions>
        <BaseButton @click="closeEditor">
          Cancel
        </BaseButton>
        <BaseButton
          variant="primary"
          type="submit"
          form="key-editor"
          :busy="saving"
        >
          {{ editing === null ? 'Create' : 'Save' }}
        </BaseButton>
      </template>
    </BaseModal>

    <ConflictDialog
      :open="conflictOpen"
      :key-name="editing?.key ?? ''"
      :your-value="formValue"
      :latest="conflictLatest"
      :busy="conflictBusy"
      :error="conflictError"
      @overwrite="conflictOverwrite"
      @reload="conflictReload"
      @cancel="conflictOpen = false"
    />

    <BaseModal
      :open="deleting !== null"
      title="Delete key"
      @close="deleting = null"
    >
      <p>
        Delete <strong class="mono">{{ deleting?.key }}</strong>? This cannot be undone.
      </p>
      <p
        v-if="deleteError !== ''"
        class="form-error"
        role="alert"
      >
        {{ deleteError }}
      </p>
      <template #actions>
        <BaseButton @click="deleting = null">
          Cancel
        </BaseButton>
        <BaseButton
          variant="danger"
          :busy="deleteBusy"
          @click="confirmDelete"
        >
          Delete
        </BaseButton>
      </template>
    </BaseModal>
  </section>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 1rem;
  flex-wrap: wrap;
}
.spacer {
  flex: 1;
}
.feed {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.9em;
  color: var(--text-muted);
}
.dot {
  width: 0.6rem;
  height: 0.6rem;
  border-radius: 50%;
  background: var(--border-strong);
}
.feed-live .dot,
.feed-fallback .dot {
  background: var(--success);
}
.feed-reconnecting .dot,
.feed-connecting .dot {
  background: var(--focus);
}
.feed-stopped .dot {
  background: var(--danger);
}
.live-notice {
  border-left: 4px solid var(--focus);
  padding: 0.4rem 0.6rem;
  background: var(--bg);
}
h1 {
  font-size: 1.5rem;
  margin: 0.5rem 0;
}
.filter {
  display: flex;
  align-items: flex-end;
  gap: 0.75rem;
  flex-wrap: wrap;
  margin-top: 0.75rem;
}
.filter :deep(.field) {
  flex: 1 1 18rem;
  margin-bottom: 0;
}
.filter-actions {
  display: flex;
  gap: 0.5rem;
  padding-bottom: 1.6rem;
}
.status {
  color: var(--text-muted);
  min-height: 1.4em;
}
.keys {
  width: 100%;
  border-collapse: collapse;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.keys th,
.keys td {
  text-align: left;
  padding: 0.5rem 0.75rem;
  border-bottom: 1px solid var(--border);
  vertical-align: middle;
}
.keys tbody th {
  font-weight: 600;
}
.mono {
  font-family: var(--font-mono);
}
.value {
  word-break: break-all;
}
.row-actions {
  display: flex;
  gap: 0.4rem;
  justify-content: flex-end;
}
.empty,
.editing-note {
  color: var(--text-muted);
}
.load-error,
.form-error {
  color: var(--danger);
}
</style>
