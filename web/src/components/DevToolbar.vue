<script setup lang="ts">
import type { ConnectionState } from '@/types'
import { cycleTyping, setConnection, setTheme, state, typingIn } from '@/store'

/**
 * Harness, not product. The canvas draws the connection and theme states as
 * separate boards; the app has to be able to *enter* them, and until there is
 * a server to disconnect from that means a switch. Delete this component the
 * day a real transport lands.
 */
const states: ConnectionState[] = ['live', 'reconnecting', 'resyncing', 'offline']
</script>

<template>
  <div class="dev">
    <span class="tag">HARNESS</span>
    <button
      v-for="s in states"
      :key="s"
      :class="{ on: state.connection.state === s }"
      @click="setConnection(s)"
    >
      {{ s.toUpperCase() }}
    </button>
    <span class="sep" />
    <!-- The canvas ships three typing cards; all three need to be reachable. -->
    <button
      :class="{ on: typingIn(state.activeId).length > 0 }"
      @click="cycleTyping"
    >
      TYPING {{ typingIn(state.activeId).length }}
    </button>
    <span class="sep" />
    <button
      :class="{ on: state.theme === 'dark' }"
      @click="setTheme('dark')"
    >
      DARK
    </button>
    <button
      :class="{ on: state.theme === 'light' }"
      @click="setTheme('light')"
    >
      LIGHT
    </button>
  </div>
</template>

<style scoped>
.dev {
  position: fixed;
  right: 12px;
  bottom: 12px;
  z-index: 10;
  display: flex;
  gap: 10px;
  align-items: center;
  padding: 6px 10px;
  background: var(--surface-rail);
  border: 1px solid var(--line-hair);
}

.dev button,
.tag {
  font: var(--type-label);
  letter-spacing: 0.12em;
  color: var(--text-meta);
}

.tag {
  color: var(--text-dim);
}

.dev button.on {
  color: var(--accent-wire);
}

.sep {
  width: 1px;
  height: 12px;
  background: var(--line-hair);
}

@media (max-width: 900px) {
  .dev {
    right: 8px;
    bottom: 8px;
    gap: 8px;
  }
}
</style>
