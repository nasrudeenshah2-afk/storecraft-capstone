<script setup lang="ts">
import { computed } from 'vue'
import BaseButton from './BaseButton.vue'
import BaseModal from './BaseModal.vue'

const props = withDefaults(
  defineProps<{
    open: boolean
    keyName: string
    /** The text the user tried to save. */
    yourValue: string
    /** The key as it is now on the server, or null if someone deleted it. */
    latest: { value: unknown; version: number } | null
    busy?: boolean
    error?: string
    /** Dialog title; defaults to the key conflict title. */
    title?: string
    /** What is being compared: "value" (a key) or, for example, "values" (a quota). */
    noun?: string
    /** First sentence; defaults to the key conflict explanation. */
    intro?: string
  }>(),
  { busy: false, error: '', title: '', noun: 'value', intro: '' },
)

const emit = defineEmits<{
  /** Save my value anyway, on top of the latest version. */
  overwrite: []
  /** Discard my text and continue from the latest value. */
  reload: []
  /** Close this dialog and go back to my text. */
  cancel: []
}>()

const latestText = computed(() => (props.latest === null ? '' : (JSON.stringify(props.latest.value, null, 2) ?? 'null')))
const dialogTitle = computed(() => (props.title !== '' ? props.title : `Conflict: "${props.keyName}" changed`))
const introText = computed(() => {
  if (props.intro !== '') return props.intro
  return props.latest !== null
    ? 'Someone else saved this key while you were editing. Compare the two values and choose what to keep.'
    : 'Someone else deleted this key while you were editing.'
})
</script>

<template>
  <BaseModal
    :open="open"
    :title="dialogTitle"
    @close="emit('cancel')"
  >
    <p>{{ introText }}</p>

    <div class="compare">
      <section aria-labelledby="conflict-yours">
        <h3 id="conflict-yours">
          Your {{ noun }}
        </h3>
        <pre class="value">{{ yourValue }}</pre>
      </section>
      <section aria-labelledby="conflict-latest">
        <h3 id="conflict-latest">
          {{ latest === null ? `Latest ${noun}` : `Latest ${noun} (version ${latest.version})` }}
        </h3>
        <pre
          v-if="latest !== null"
          class="value"
        >{{ latestText }}</pre>
        <p
          v-else
          class="deleted"
        >
          (deleted)
        </p>
      </section>
    </div>

    <p
      v-if="error !== ''"
      class="error"
      role="alert"
    >
      {{ error }}
    </p>

    <template #actions>
      <BaseButton @click="emit('cancel')">
        Cancel
      </BaseButton>
      <BaseButton
        :disabled="busy"
        @click="emit('reload')"
      >
        {{ latest === null ? 'Discard my edit' : 'Reload latest' }}
      </BaseButton>
      <BaseButton
        variant="primary"
        :busy="busy"
        @click="emit('overwrite')"
      >
        {{ latest === null ? `Re-create with my ${noun}` : `Overwrite with my ${noun}` }}
      </BaseButton>
    </template>
  </BaseModal>
</template>

<style scoped>
.compare {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
  gap: 1rem;
}
h3 {
  font-size: 1rem;
  margin: 0 0 0.4rem;
}
.value {
  font-family: var(--font-mono);
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 0.6rem;
  margin: 0;
  max-height: 14rem;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.deleted {
  color: var(--text-muted);
  font-style: italic;
}
.error {
  color: var(--danger);
}
</style>
