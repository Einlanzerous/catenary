<script setup lang="ts">
import { directs, openSearch, rooms, state, totalUnread, user } from '@/store'
import Avatar from './Avatar.vue'
import ConversationRow from './ConversationRow.vue'

/**
 * Deliberate call 04: one list, two headers. Rooms above DMs in the same
 * column with the same row anatomy — no separate server rail, because with
 * thirty people there is nothing to navigate between.
 */
const me = user(state.me)
</script>

<template>
  <nav class="rail">
    <header class="brand">
      <span class="mark" />
      <span class="wordmark">CATENARY</span>
      <span class="who">
        {{ me?.name.split(' ')[0].toUpperCase() }} · {{ totalUnread }}
      </span>
    </header>

    <div class="search-slot">
      <button class="search" @click="openSearch">
        <span class="glyph">⌕</span>
        <span class="placeholder">Search messages, transcripts</span>
        <!-- CTRL, not ⌘: iOS and macOS are out of scope, and a key cap
             nobody's keyboard has is worse than none. -->
        <span class="key">CTRL+K</span>
      </button>
    </div>

    <div class="list scroll">
      <div class="section">
        <span class="label">ROOMS</span>
        <span class="count meta tnum">{{ rooms.length }}</span>
      </div>
      <ConversationRow
        v-for="c in rooms"
        :key="c.id"
        :conversation="c"
      />

      <div class="section spaced">
        <span class="label">DIRECT</span>
        <span class="count meta tnum">{{ directs.length }}</span>
      </div>
      <ConversationRow
        v-for="c in directs"
        :key="c.id"
        :conversation="c"
      />
    </div>

    <footer class="me">
      <Avatar :user-id="state.me" :size="22" />
      <span class="name">{{ me?.name }}</span>
      <span class="link-state">LINKED</span>
      <span class="live" :class="state.connection.state" />
    </footer>
  </nav>
</template>

<style scoped>
.rail {
  display: grid;
  grid-template-rows: 52px 48px 1fr 52px;
  overflow: hidden;
  background: var(--surface-rail);
  border-right: 1px solid var(--line-hair);
}

.brand {
  display: flex;
  gap: 10px;
  align-items: center;
  padding: 0 16px 0 20px;
  border-bottom: 1px solid var(--line-hair);
}

.mark {
  display: block;
  width: 9px;
  height: 9px;
  background: var(--accent-wire);
}

.wordmark {
  font-family: var(--font-mono);
  font-size: 12px;
  letter-spacing: 0.2em;
  color: var(--text-primary);
}

.who {
  margin-left: auto;
  font: var(--type-label);
  letter-spacing: 0.1em;
  color: var(--text-meta);
}

.search-slot {
  padding: 8px 12px;
  border-bottom: 1px solid var(--line-hair);
}

.search {
  display: flex;
  gap: 7px;
  align-items: center;
  width: 100%;
  height: 32px;
  padding: 0 8px;
  background: var(--surface-base);
  border: 1px solid var(--line-edge);
  border-radius: var(--radius-input);
}

.search .glyph {
  flex: none;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-dim);
}

.search .placeholder {
  overflow: hidden;
  font-size: 12.5px;
  color: var(--text-meta);
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* The cap is wider than ⌘K was, so it is pinned and the label ellipses
 * rather than the shortcut wrapping. */
.search .key {
  flex: none;
  margin-left: auto;
  padding: 1px 4px;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.04em;
  color: var(--text-dim);
  border: 1px solid var(--line-edge);
  white-space: nowrap;
}

.list {
  min-height: 0;
}

.section {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 20px 8px;
}
.section.spaced {
  padding-top: 22px;
}

.section .label {
  letter-spacing: 0.16em;
}

.count {
  font-size: 10px;
  color: var(--text-dim);
}

.me {
  display: flex;
  gap: 10px;
  align-items: center;
  padding: 0 20px;
  border-top: 1px solid var(--line-hair);
}

.me .name {
  font-size: 13px;
  color: var(--text-primary);
}

.link-state {
  margin-left: auto;
  font: var(--type-label);
  letter-spacing: 0.1em;
  color: var(--text-meta);
}

/* The rail's own connection lamp — same accent, no second color. */
.live {
  display: block;
  width: 6px;
  height: 6px;
  background: var(--accent-wire);
}
.live.reconnecting,
.live.resyncing {
  animation: catPulse 1.2s ease-in-out infinite;
}
.live.offline {
  background: var(--text-disabled);
}
</style>
