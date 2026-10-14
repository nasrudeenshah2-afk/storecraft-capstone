<script setup lang="ts">
import { computed, useId } from 'vue'

const props = withDefaults(
  defineProps<{
    label: string
    /** One line (input) or several (textarea). */
    multiline?: boolean
    rows?: number
    type?: 'text' | 'password'
    /** Validation message; also marks the field invalid for screen readers. */
    error?: string
    /** Help text shown under the field. */
    hint?: string
    required?: boolean
    disabled?: boolean
    autocomplete?: string
    monospace?: boolean
  }>(),
  {
    multiline: false,
    rows: 4,
    type: 'text',
    error: '',
    hint: '',
    required: false,
    disabled: false,
    autocomplete: 'off',
    monospace: false,
  },
)

const model = defineModel<string>({ required: true })

const id = useId()
const hintId = `${id}-hint`
const errorId = `${id}-error`
const describedBy = computed(() => {
  const ids: string[] = []
  if (props.hint !== '') ids.push(hintId)
  if (props.error !== '') ids.push(errorId)
  return ids.length > 0 ? ids.join(' ') : undefined
})
</script>

<template>
  <div class="field">
    <label :for="id">{{ label }}<span
      v-if="required"
      aria-hidden="true"
    > *</span></label>
    <textarea
      v-if="multiline"
      :id="id"
      v-model="model"
      :rows="rows"
      :required="required"
      :disabled="disabled"
      :aria-invalid="error !== '' ? 'true' : undefined"
      :aria-describedby="describedBy"
      :class="{ mono: monospace }"
      spellcheck="false"
    />
    <input
      v-else
      :id="id"
      v-model="model"
      :type="type"
      :required="required"
      :disabled="disabled"
      :autocomplete="autocomplete"
      :aria-invalid="error !== '' ? 'true' : undefined"
      :aria-describedby="describedBy"
      :class="{ mono: monospace }"
      spellcheck="false"
    >
    <p
      v-if="hint !== ''"
      :id="hintId"
      class="hint"
    >
      {{ hint }}
    </p>
    <p
      v-if="error !== ''"
      :id="errorId"
      class="error"
    >
      {{ error }}
    </p>
  </div>
</template>

<style scoped>
.field {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  margin-bottom: 0.9rem;
}
label {
  font-weight: 600;
}
input,
textarea {
  font: inherit;
  padding: 0.45rem 0.55rem;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  background: var(--surface);
  color: var(--text);
  width: 100%;
}
.mono {
  font-family: var(--font-mono);
}
[aria-invalid='true'] {
  border-color: var(--danger);
}
.hint {
  margin: 0;
  color: var(--text-muted);
  font-size: 0.9em;
}
.error {
  margin: 0;
  color: var(--danger);
  font-size: 0.9em;
}
</style>
