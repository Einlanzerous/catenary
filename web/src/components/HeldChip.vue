<script setup lang="ts">
/**
 * CANT-330 (canvas 08, section 1). What the composer is holding for SEND: the
 * link preview being fetched or fetched, and the picked image. One component
 * with two tags, so the two stack in a shared tag column and read as the same
 * kind of thing. The tag and the filename or host are the person's own and
 * the app's; the title is the page's own words, drawn as text.
 *
 * NO ACCENT. Nothing on a chip is copper; FETCHING and its sweep are
 * text.meta, and motion is colour and the one sweep, no transform on the chip.
 */
defineProps<{
  tag: 'LINK' | 'IMAGE'
  /** The host (link) or filename (image), mono. */
  name: string
  /** The page's title. Absent while it is still being fetched. */
  title?: string
  fetching?: boolean
  dismissLabel: string
}>()
defineEmits<{ dismiss: [] }>()
</script>

<template>
  <div class="chip" :class="{ fetching }" :data-chip="tag.toLowerCase()">
    <span class="tag">{{ tag }}</span>
    <span class="body">
      <span class="name">{{ name }}</span>
      <span v-if="fetching" class="wait">FETCHING PREVIEW</span>
      <span v-else-if="title" class="title">{{ title }}</span>
    </span>
    <button class="x" type="button" :aria-label="dismissLabel" @click="$emit('dismiss')">✕</button>
    <span v-if="fetching" class="sweep" aria-hidden="true" />
  </div>
</template>

<style scoped>
.chip {
  position: relative;
  display: flex;
  align-items: stretch;
  max-width: 520px;
  min-width: 0;
  height: 32px;
  overflow: hidden;
  background: var(--surface-rail);
  border: 1px solid var(--line-hair);
}

.tag {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: center;
  width: 60px;
  font-family: var(--font-mono);
  font-size: 10px;
  letter-spacing: 0.14em;
  color: var(--text-secondary);
  border-right: 1px solid var(--line-hair);
}

.body {
  display: flex;
  flex: 1;
  gap: 8px;
  align-items: center;
  min-width: 0;
  padding: 0 10px;
}

.name {
  flex: 0 1 auto;
  min-width: 0;
  overflow: hidden;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-secondary);
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* Only a link with a title shares the line; a filename may take all of it. */
.name:only-child {
  flex: 1 1 auto;
}
.body:has(.title, .wait) .name {
  max-width: 60%;
}

.title,
.wait {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.title {
  font-size: 13px;
  line-height: 18px;
  color: var(--text-primary);
}
.wait {
  font-family: var(--font-mono);
  font-size: 10px;
  letter-spacing: 0.12em;
  color: var(--text-meta);
}

.x {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: center;
  width: 32px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-secondary);
  border-left: 1px solid var(--line-hair);
  transition:
    background-color 80ms linear,
    color 80ms linear;
}
.x:hover {
  color: var(--text-primary);
  background: var(--surface-raised);
}
.x:focus-visible {
  /* TODO(CANT-332): a focus-ring token; the canvas's interim is text.primary. */
  outline: 2px solid var(--text-primary);
  outline-offset: -2px;
}

/* The fetch sweep (canvas: a 1px text.meta rule, 24% wide, linear 1.4 s). It
 * is the one thing that moves, and it never changes the chip's box. */
.sweep {
  position: absolute;
  bottom: 0;
  left: 0;
  width: 24%;
  height: 1px;
  background: var(--text-meta);
  animation: catSweep 1.4s linear infinite;
}

/* Narrow (canvas 1E): 44px high, host over title, 58px tag, 44px target. */
@media (max-width: 900px) {
  .chip {
    max-width: none;
    height: 44px;
  }
  .tag {
    width: 58px;
    font-size: 9.5px;
  }
  .body {
    flex-direction: column;
    gap: 0;
    align-items: stretch;
    justify-content: center;
  }
  .body:has(.title, .wait) .name {
    max-width: none;
    font-size: 10.5px;
    line-height: 14px;
  }
  .name:only-child {
    flex: 0 0 auto;
    font-size: 10.5px;
  }
  .wait {
    font-size: 9.5px;
    line-height: 18px;
  }
  .x {
    width: 44px;
    font-size: 12px;
  }
}
</style>
