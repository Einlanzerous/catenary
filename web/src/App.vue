<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { activeConversation, closeSearch, openSearch, state } from '@/store'
import AccountView from './components/AccountView.vue'
import NewConversation from './components/NewConversation.vue'
import Rail from './components/Rail.vue'
import SearchView from './components/SearchView.vue'
import Thread from './components/Thread.vue'

/** The views a rail control opens in the main pane. */
const mainPaneViews: ReadonlySet<string> = new Set(['account', 'new', 'search'])

/** Below 900px the rail is a full-screen first pane rather than a column. */
const railOpen = ref(!mainPaneViews.has(state.view))

watch(
  () => state.activeId,
  () => (railOpen.value = false),
)

/** The picker, the account view and search are main-pane views; below 900px
 *  that means leaving the rail. For the account view this is also the sign-in
 *  form (CANT-281): a visitor with no credential has no conversation to open
 *  and nothing in the rail to tap, so the form has to be what is on screen,
 *  not a pane hidden behind a rail with nothing in it. Search was missing from
 *  this list, so at phone width the search control and Ctrl+K opened a view
 *  nobody could see (CANT-282). */
watch(
  () => state.view,
  (view) => {
    if (mainPaneViews.has(view)) railOpen.value = false
  },
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
    <Rail class="pane-rail" @reveal="railOpen = false" />
    <SearchView v-if="state.view === 'search'" class="pane-main" />
    <AccountView v-else-if="state.view === 'account'" class="pane-main" />
    <NewConversation v-else-if="state.view === 'new'" class="pane-main" />
    <Thread v-else-if="activeConversation" class="pane-main" />
    <!-- Nothing to open yet: no device enrolled, or the first page has not
         landed. An empty pane rather than an invented conversation. -->
    <main v-else class="pane-main empty" />

    <button class="back-to-rail" @click="railOpen = true">‹ ALL</button>
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
