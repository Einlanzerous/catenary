<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { duration, fullStamp } from '@/lib/format'
import {
  closeSearch,
  hitCounts,
  jumpTo,
  searchHits,
  state,
  user,
  voiceOf,
  type SearchHit,
} from '@/store'
import Avatar from './Avatar.vue'
import Waveform from './Waveform.vue'

/** Text and transcripts in one list. Deliberate call 18: TXT and VOX rows
 *  share the same match line and grid; only a three-letter tag separates
 *  them. */
type Filter = 'all' | 'text' | 'voice' | 'images'
const filter = ref<Filter>('all')

const box = ref<HTMLInputElement | null>(null)
onMounted(() => box.value?.focus())

const shown = computed(() =>
  searchHits.value.filter((h) =>
    filter.value === 'all'
      ? true
      : filter.value === 'text'
        ? h.type === 'TXT'
        : filter.value === 'voice'
          ? h.type === 'VOX'
          : false,
  ),
)

/** Splits a snippet around the query so the match can take the wash without
 *  going anywhere near v-html. */
function parts(text: string): { text: string; hit: boolean }[] {
  const q = state.query.trim()
  if (!q) return [{ text, hit: false }]
  const out: { text: string; hit: boolean }[] = []
  const lower = text.toLowerCase()
  const needle = q.toLowerCase()
  let i = 0
  for (;;) {
    const at = lower.indexOf(needle, i)
    if (at < 0) break
    if (at > i) out.push({ text: text.slice(i, at), hit: false })
    out.push({ text: text.slice(at, at + q.length), hit: true })
    i = at + q.length
  }
  if (i < text.length) out.push({ text: text.slice(i), hit: false })
  return out
}

const voiceFor = (hit: SearchHit) => voiceOf(hit.message)

function open(hit: SearchHit) {
  jumpTo(hit.message.id, hit.jumpToSec)
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') closeSearch()
}
</script>

<template>
  <section class="search">
    <header class="head">
      <div class="field">
        <span class="glyph">⌕</span>
        <input
          ref="box"
          v-model="state.query"
          class="input"
          placeholder="Search messages and transcripts"
          @keydown="onKeydown"
        />
        <span class="count meta tnum">
          {{ hitCounts.all }} RESULTS · 0.04 S
        </span>
      </div>

      <div class="filters">
        <button
          v-for="f in (['all', 'text', 'voice', 'images'] as Filter[])"
          :key="f"
          class="chip"
          :class="{ on: filter === f }"
          @click="filter = f"
        >
          {{ f.toUpperCase() }} {{ hitCounts[f] }}
        </button>
        <span class="facet">FROM: ANYONE</span>
        <span class="facet">IN: ANY ROOM</span>
        <span class="facet last">SORT: NEWEST</span>
        <button class="facet close" @click="closeSearch">ESC CLOSES</button>
      </div>
    </header>

    <div class="columns">
      <span>TYPE</span><span>MATCH</span><span class="right">WHERE · WHEN</span>
    </div>

    <div class="results scroll">
      <button
        v-for="hit in shown"
        :key="hit.message.id"
        class="hit"
        :class="{ vox: hit.type === 'VOX' }"
        @click="open(hit)"
      >
        <span class="tag" :class="hit.type.toLowerCase()">{{ hit.type }}</span>

        <span class="match">
          <span class="who">
            <Avatar :user-id="hit.message.authorId" :size="18" />
            <span class="name">{{
              hit.message.authorId === state.me ? 'You' : user(hit.message.authorId)?.name
            }}</span>
            <span
              v-if="hit.type === 'VOX'"
              class="note"
              :class="{ pending: hit.notSearchableYet }"
            >
              <template v-if="hit.notSearchableYet"
                >TRANSCRIPT PENDING · NOT SEARCHABLE YET</template
              >
              <template v-else
                >TRANSCRIPT MATCH ·
                {{ duration(voiceFor(hit)?.durationSec ?? 0) }}</template
              >
            </span>
          </span>

          <span
            v-if="hit.type === 'VOX'"
            class="vox-line"
            :class="{ dim: hit.notSearchableYet }"
          >
            <span class="mini-play">▶</span>
            <Waveform
              class="mini-wave"
              :peaks="voiceFor(hit)?.peaks ?? []"
              :bars="36"
              :height="22"
              dim
            />
            <span class="snippet secondary">
              <template v-if="hit.notSearchableYet">{{
                `matched on filename and sender only · ${duration(voiceFor(hit)?.durationSec ?? 0)}`
              }}</template>
              <template v-else>
                <span
                  v-for="(part, i) in parts(hit.snippet)"
                  :key="i"
                  :class="{ hitmark: part.hit }"
                  >{{ part.text }}</span
                >
              </template>
            </span>
          </span>

          <span v-else class="snippet body">
            <span
              v-for="(part, i) in parts(hit.snippet)"
              :key="i"
              :class="{ hitmark: part.hit }"
              >{{ part.text }}</span
            >
          </span>
        </span>

        <span class="where meta tnum">
          <span class="room">{{ hit.conversation.name }}</span>
          <span>{{ fullStamp(hit.message.at) }}</span>
          <!-- Seeks the audio to the matched word, not just the message. -->
          <span v-if="hit.jumpToSec !== undefined" class="jump">
            JUMP TO {{ duration(hit.jumpToSec) }}
          </span>
        </span>
      </button>

      <p v-if="state.query && !shown.length" class="empty">
        No matches for “{{ state.query }}”.
      </p>
      <p v-else-if="!state.query" class="empty">
        Type to search message text and voice-note transcripts together.
      </p>
    </div>
  </section>
</template>

<style scoped>
.search {
  display: grid;
  grid-template-rows: auto auto 1fr;
  min-width: 0;
  overflow: hidden;
  background: var(--surface-base);
}

.head {
  padding: 16px var(--s6) 14px;
  background: var(--surface-rail);
  border-bottom: 1px solid var(--line-hair);
}

.field {
  display: flex;
  gap: 10px;
  align-items: center;
  max-width: 720px;
  height: 38px;
  padding: 0 12px;
  background: var(--surface-base);
  border: 1px solid var(--accent-wire);
  border-radius: var(--radius-input);
}

.glyph {
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--accent-wire);
}

.input {
  flex: 1;
  min-width: 0;
  font: var(--type-body);
  color: var(--text-primary);
  background: none;
  border: 0;
  outline: none;
}

.field .count {
  flex: none;
  font-size: 10px;
}

.filters {
  display: flex;
  gap: 8px;
  align-items: center;
  margin-top: 12px;
}

.chip {
  padding: 4px 10px;
  font: var(--type-label);
  letter-spacing: 0.12em;
  color: var(--text-secondary);
  border: 1px solid var(--line-edge);
}
.chip.on {
  color: var(--on-accent);
  background: var(--accent-wire);
  border-color: var(--accent-wire);
}

.facet {
  font: var(--type-label);
  letter-spacing: 0.12em;
  color: var(--text-meta);
}
.facet.last {
  margin-left: auto;
}
.facet.close {
  color: var(--text-dim);
}

.columns {
  display: grid;
  grid-template-columns: 64px 1fr 148px;
  column-gap: var(--s4);
  padding: 10px var(--s6);
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: var(--track-label);
  color: var(--text-meta);
  border-bottom: 1px solid var(--line-hair);
}

.right {
  text-align: right;
}

.results {
  min-height: 0;
}

.hit {
  display: grid;
  grid-template-columns: 64px 1fr 148px;
  column-gap: var(--s4);
  width: 100%;
  padding: 14px var(--s6);
  text-align: left;
  border-bottom: 1px solid var(--line-faint);
}

/* Call 19: a one-step lift as a zebra cue. Debatable — it does subtly
 * privilege voice rows. Delete this rule to flatten it. */
.hit.vox {
  background: var(--surface-lift);
}

.hit:hover {
  background: var(--surface-raised);
}

.tag {
  align-self: start;
  justify-self: start;
  padding: 2px 6px;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.1em;
  color: var(--text-secondary);
  border: 1px solid var(--surface-avatar);
}
.tag.vox {
  color: var(--accent-wire);
  border-color: var(--line-accent-dim);
}

.match {
  display: block;
  min-width: 0;
}

.who {
  display: flex;
  gap: 9px;
  align-items: center;
}

.name {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-primary);
}

.note {
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.1em;
  color: var(--text-meta);
}
.note.pending {
  color: var(--accent-dim);
}

.snippet {
  display: block;
  max-width: 88ch;
  margin-top: 4px;
  font: var(--type-body);
  line-height: 24px;
  color: var(--text-primary);
}

.snippet.secondary {
  max-width: 70ch;
  margin-top: 0;
  color: var(--text-secondary);
}

.hitmark {
  color: var(--text-primary);
  background: var(--accent-wash);
  box-shadow: inset 0 -1px 0 var(--accent-underline);
}

.vox-line {
  display: flex;
  gap: 12px;
  align-items: center;
  margin-top: 6px;
}
.vox-line.dim {
  opacity: 0.62;
}

.mini-play {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  font-size: 9px;
  color: var(--on-accent);
  background: var(--accent-wire);
}
.vox-line.dim .mini-play {
  background: var(--text-disabled);
}

.mini-wave {
  flex: none;
  width: 150px;
}

.where {
  display: grid;
  gap: 2px;
  align-content: start;
  font-size: 10.5px;
  line-height: 1.6;
  text-align: right;
}

.where .room {
  color: var(--text-secondary);
}

.jump {
  color: var(--accent-wire);
}

.empty {
  padding: 32px var(--s6);
  color: var(--text-meta);
}

@media (max-width: 900px) {
  .hit,
  .columns {
    grid-template-columns: 48px 1fr;
    padding: 12px var(--s4);
  }
  .where,
  .columns .right {
    display: none;
  }
  .head {
    padding: 12px var(--s4);
  }
}
</style>
