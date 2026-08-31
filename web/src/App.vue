<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { closeSearch, openSearch, state } from '@/store'
import DevToolbar from './components/DevToolbar.vue'
import Rail from './components/Rail.vue'
import SearchView from './components/SearchView.vue'
import Thread from './components/Thread.vue'

/** Below 900px the rail is a full-screen first pane rather than a column. */
const railOpen = ref(true)

watch(
  () => state.activeId,
  () => (railOpen.value = false),
)

/** Ctrl+K, and only Ctrl+K — the key cap in the rail says so, and a second
 *  undocumented binding is how the two clients start to disagree. */
function onKeydown(event: KeyboardEvent) {
  if (event.ctrlKey && !event.altKey && event.key.toLowerCase() === 'k') {
    event.preventDefault()
    state.view === 'search' ? closeSearch() : openSearch()
  }
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onUnmounted(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="app" :class="{ 'rail-open': railOpen }">
    <Rail class="pane-rail" />
    <SearchView v-if="state.view === 'search'" class="pane-main" />
    <Thread v-else class="pane-main" />

    <button class="back-to-rail" @click="railOpen = true">‹ ALL</button>

    <DevToolbar />
  </div>
</template>

<style scoped>
.app {
  display: grid;
  grid-template-columns: var(--rail-w) 1fr;
  height: 100%;
  overflow: hidden;
}

.back-to-rail {
  display: none;
}

@media (max-width: 900px) {
  .app {
    grid-template-columns: 1fr;
  }

  .pane-rail {
    display: none;
  }
  .app.rail-open .pane-rail {
    display: grid;
  }
  .app.rail-open .pane-main {
    display: none;
  }

  .app:not(.rail-open) .back-to-rail {
    position: fixed;
    top: 0;
    left: 0;
    z-index: 5;
    display: block;
    height: 52px;
    padding: 0 12px;
    font: var(--type-label);
    letter-spacing: var(--track-label);
    color: var(--text-meta);
  }

  .app:not(.rail-open) .pane-main :deep(.head) {
    padding-left: 64px;
  }
}
</style>
