<script setup lang="ts">
import { ref } from 'vue'
import { createApiClient } from '../api/client'
import { describeError } from '../api/errors'
import BaseButton from '../components/BaseButton.vue'
import BaseTextField from '../components/BaseTextField.vue'
import { useToast } from '../composables/useToast'
import { useSessionStore, type Role } from '../stores/session'

const session = useSessionStore()
const toast = useToast()

const token = ref('')
const role = ref<Role>('tenant')
const tokenError = ref('')
const formError = ref('')
const busy = ref(false)

/**
 * Checks the token against the server before signing in: a tenant token must
 * be able to list keys, an admin token must be able to read the replication
 * status (an admin-only route).
 */
async function submit(): Promise<void> {
  tokenError.value = ''
  formError.value = ''
  const candidate = token.value.trim()
  if (candidate === '') {
    tokenError.value = 'Paste your token.'
    return
  }
  busy.value = true
  const api = createApiClient({ getToken: () => candidate })
  try {
    if (role.value === 'tenant') {
      await api.listKeys()
    } else {
      await api.replicationStatus()
    }
  } catch (err) {
    formError.value = describeError(err, {
      unauthorized: 'The server did not accept this token. Check that you pasted all of it.',
      forbidden:
        role.value === 'tenant'
          ? 'This is an admin token. Choose "Admin", or paste a tenant token.'
          : 'This is a tenant token. Choose "Tenant", or paste the admin token.',
    })
    return
  } finally {
    busy.value = false
  }
  session.login(candidate, role.value)
  token.value = ''
  toast.success(role.value === 'tenant' ? 'Signed in as tenant.' : 'Signed in as admin.')
}
</script>

<template>
  <section
    class="login"
    aria-labelledby="login-heading"
  >
    <h1 id="login-heading">
      Sign in
    </h1>
    <form
      novalidate
      @submit.prevent="submit"
    >
      <BaseTextField
        v-model="token"
        label="Token"
        type="password"
        required
        monospace
        :error="tokenError"
        hint="Paste the token printed by 'storecraft seed', or the admin token. It is kept only for this browser tab."
      />
      <fieldset class="roles">
        <legend>Sign in as</legend>
        <label><input
          v-model="role"
          type="radio"
          name="role"
          value="tenant"
        > Tenant</label>
        <label><input
          v-model="role"
          type="radio"
          name="role"
          value="admin"
        > Admin</label>
      </fieldset>
      <p
        v-if="formError !== ''"
        class="form-error"
        role="alert"
      >
        {{ formError }}
      </p>
      <BaseButton
        variant="primary"
        type="submit"
        :busy="busy"
      >
        Sign in
      </BaseButton>
    </form>
  </section>
</template>

<style scoped>
.login {
  max-width: 28rem;
  margin: 2rem auto;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 1.5rem;
}
h1 {
  margin-top: 0;
  font-size: 1.5rem;
}
.roles {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  margin: 0 0 1rem;
  padding: 0.5rem 0.9rem 0.75rem;
  display: flex;
  gap: 1.5rem;
}
.roles legend {
  font-weight: 600;
  padding: 0 0.3rem;
}
.form-error {
  color: var(--danger);
}
</style>
