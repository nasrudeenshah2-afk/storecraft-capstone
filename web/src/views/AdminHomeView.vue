<script setup lang="ts">
import QuotaEnforcementPanel from '../components/QuotaEnforcementPanel.vue'
import TenantQuotaEditor from '../components/TenantQuotaEditor.vue'
import { useFlagsStore } from '../stores/flags'

// The replication badge lives in the app header (App.vue). The quota editor is
// shown only while quota_enforcement is on; the flag switch is always shown.
const flags = useFlagsStore()
</script>

<template>
  <section
    aria-labelledby="admin-heading"
    class="admin"
  >
    <h1 id="admin-heading">
      Signed in as admin
    </h1>
    <p>
      The admin token cannot read or change tenant keys. To manage keys, sign out and sign in with a tenant token.
    </p>
    <p class="muted">
      The badge at the top shows this server's replication status (role, applied and primary seq, lag); it refreshes every 5 seconds.
    </p>
  </section>
  <QuotaEnforcementPanel />
  <TenantQuotaEditor v-if="flags.quotaEnabled" />
</template>

<style scoped>
.admin {
  max-width: 40rem;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 1.5rem;
}
h1 {
  margin-top: 0;
  font-size: 1.5rem;
}
.muted {
  color: var(--text-muted);
}
</style>
