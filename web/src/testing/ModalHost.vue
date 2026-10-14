<script setup lang="ts">
// Test-only host page for BaseModal: an opener button and a modal with two buttons.
import { ref } from 'vue'
import BaseModal from '../components/BaseModal.vue'

const open = ref(false)
</script>

<template>
  <button
    id="opener"
    type="button"
    @click="open = true"
  >
    Open
  </button>
  <BaseModal
    :open="open"
    title="Example"
    @close="open = false"
  >
    <button
      id="first"
      type="button"
    >
      First
    </button>
    <template #actions>
      <button
        id="last"
        type="button"
      >
        Last
      </button>
    </template>
  </BaseModal>
</template>
