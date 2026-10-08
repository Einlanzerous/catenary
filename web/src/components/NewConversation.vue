<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { closeNew } from '@/store'
import { pick, pickerState, resetPicker, retryPick } from '@/conversations'

/**
 * CANT-270 — the new-conversation picker, in the main pane the way search and
 * the account view are. A list of the people `GET /users` names; picking one
 * finds-or-creates the direct with them and opens it. Neither design canvas
 * draws this flow, so it is the rail's own row anatomy and the account view's
 * measure, in the same tokens, and it takes no copper beyond what a focused
 * control already does.
 *
 * Built to be extended by CANT-271 rather than rewritten: the list is a
 * `v-for` over entries with one click handler (`pick`), so a selection set, a
 * name field and a CREATE button are additions around it.
 */

/** A create is in flight, or its conversation has been returned and is on its
 *  way from the journal: nothing else may be started meanwhile. */
const locked = computed(() => pickerState.busy || pickerState.opening !== '')

// Never during SSR (onMounted does not run under renderToString), so the
// smoke drives `resetPicker` itself, as it drives the account view's checks.
onMounted(resetPicker)
</script>

<template>
  <section class="new-conversation" aria-label="New conversation">
    <header class="head">
      <h1 class="title">NEW CONVERSATION</h1>
      <button class="close" @click="closeNew">CLOSE</button>
    </header>
    <p class="lede">Pick someone to start a direct conversation with. If you already have one, it opens.</p>

    <!-- A refusal is a message with a way forward, not an empty list. -->
    <div v-if="pickerState.error" class="error" role="alert">
      <span class="error-text">{{ pickerState.error }}</span>
      <button v-if="pickerState.failedFor" class="retry" :disabled="locked" @click="retryPick">RETRY</button>
      <button v-else class="retry" :disabled="pickerState.loading" @click="resetPicker">RETRY</button>
    </div>

    <p v-if="pickerState.busy" class="status">Starting the conversation…</p>
    <p v-else-if="pickerState.opening" class="status">Started. Waiting for the server to deliver it…</p>

    <p v-if="pickerState.roster === null && !pickerState.error" class="status">Loading people…</p>
    <p v-else-if="pickerState.roster !== null && pickerState.roster.length === 0" class="status empty">
      Nobody else has an account on this server yet.
    </p>

    <ul v-if="pickerState.roster && pickerState.roster.length > 0" class="roster">
      <li v-for="entry in pickerState.roster" :key="entry.id">
        <button class="person" :disabled="locked" :data-roster-handle="entry.handle" @click="pick(entry)">
          <span class="tile">{{ entry.initials }}</span>
          <span class="name">{{ entry.name }}</span>
          <span class="handle meta">@{{ entry.handle }}</span>
        </button>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.new-conversation {
  display: block;
  max-width: 480px;
  min-width: 0;
  padding: var(--s8) var(--s6);
  overflow: auto;
  background: var(--surface-base);
}

.head {
  display: flex;
  gap: var(--s3);
  align-items: baseline;
  justify-content: space-between;
}

.title {
  font: var(--type-title);
  color: var(--text-primary);
}

.close {
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--text-meta);
}

.lede {
  margin: var(--s2) 0 var(--s6);
  font: var(--type-secondary);
  color: var(--text-secondary);
}

.error {
  display: flex;
  gap: var(--s3);
  align-items: baseline;
  justify-content: space-between;
  padding: var(--s3) var(--s4);
  margin-bottom: var(--s4);
  border: 1px solid var(--line-edge);
}

.error-text {
  font: var(--type-secondary);
  color: var(--signal-fault);
}

.retry {
  flex: none;
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--text-primary);
}
.retry:disabled {
  color: var(--text-disabled);
  cursor: not-allowed;
}

.status {
  margin-bottom: var(--s3);
  font: var(--type-secondary);
  color: var(--text-meta);
}

.roster {
  padding: 0;
  margin: 0;
  list-style: none;
  border-top: 1px solid var(--line-hair);
}

.roster li {
  border-bottom: 1px solid var(--line-faint);
}

.person {
  display: flex;
  gap: 10px;
  align-items: center;
  width: 100%;
  padding: 9px 4px;
  text-align: left;
}
.person:hover:not(:disabled) {
  background: var(--surface-lift);
}
.person:disabled {
  cursor: not-allowed;
  opacity: 0.55;
}

/* The avatar's anatomy (Avatar.vue), from the roster's own initials: a
 * person with no conversation is not in `state.users`, so Avatar has no
 * record to read them from. Square, never a circle. */
.tile {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--text-bright);
  background: var(--surface-avatar);
}

.name {
  font: var(--type-sender);
  color: var(--text-primary);
}

.handle {
  margin-left: auto;
  font-size: 10.5px;
}
</style>
