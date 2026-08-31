<script setup lang="ts">
import { computed } from 'vue'
import type { Message, VoiceAttachment } from '@/types'
import { duration } from '@/lib/format'
import {
  cycleRate,
  isExpanded,
  progressFor,
  seek,
  state,
  toggleTranscript,
  togglePlay,
  transcriptWords,
} from '@/store'
import Waveform from './Waveform.vue'

/** The signature object. Audio is playable immediately — transcription never
 *  blocks playback, in any of the three states. */
const props = withDefaults(
  defineProps<{
    message: Message
    voice: VoiceAttachment
    maxWidth?: number
    bars?: number
    /** Search and reply contexts render a subordinate, non-expanding copy. */
    compact?: boolean
  }>(),
  { maxWidth: 620, bars: 96, compact: false },
)

const played = computed(() => progressFor(props.message.id))
const playing = computed(
  () => state.playback.messageId === props.message.id && state.playback.playing,
)
const position = computed(() => played.value * props.voice.durationSec)
const expanded = computed(() => isExpanded(props.message.id))
const words = computed(() => transcriptWords(props.voice))

/** The segment currently under the playhead, highlighted while it plays. */
const activeSegment = computed(() => {
  const segments = props.voice.transcript.segments
  if (!segments?.length || !played.value) return -1
  let index = 0
  for (let i = 0; i < segments.length; i++) {
    if (segments[i].at <= position.value) index = i
  }
  return index
})
</script>

<template>
  <div class="voice" :style="{ maxWidth: `${maxWidth}px` }">
    <div class="player">
      <button
        class="play"
        :aria-label="playing ? 'Pause' : 'Play'"
        @click="togglePlay(message.id)"
      >
        {{ playing ? '❚❚' : '▶' }}
      </button>

      <Waveform
        :peaks="voice.peaks"
        :played="played"
        :bars="bars"
        :height="compact ? 22 : 32"
        seekable
        @seek="(f) => seek(message.id, f)"
      />

      <span class="time meta tnum" :class="{ live: played > 0 }">
        <template v-if="played > 0">
          {{ duration(position) }} / {{ duration(voice.durationSec) }}
        </template>
        <template v-else>{{ duration(voice.durationSec) }}</template>
      </span>

      <button
        v-if="!compact"
        class="rate"
        :class="{ on: state.playback.rate !== 1 }"
        @click="cycleRate"
      >
        {{ state.playback.rate.toFixed(1) }}×
      </button>
    </div>

    <div v-if="!compact" class="transcript">
      <!-- A · PENDING ─────────────────────────────────────────────── -->
      <template v-if="voice.transcript.state === 'pending'">
        <div class="strip">
          <span class="dot pulse" />
          <span class="pending-label"
            >TRANSCRIBING<template v-if="voice.transcript.etaSec">
              · ~{{ voice.transcript.etaSec }} S</template
            ></span
          >
        </div>
        <!-- Two lines reserving exactly the height the collapsed transcript
             will occupy, so nothing jumps when it lands. -->
        <div class="skeleton">
          <div class="line"><i style="width: 88%" /></div>
          <div class="line"><i style="width: 64%; animation-delay: 0.3s" /></div>
        </div>
      </template>

      <!-- C · EXPANDED ────────────────────────────────────────────── -->
      <template v-else-if="expanded">
        <div class="strip">
          <span class="label">TRANSCRIPT · AUTO</span>
          <span class="rule" />
          <button class="action" @click="toggleTranscript(message.id)">
            COLLAPSE
          </button>
        </div>
        <!-- Highlights the segment currently playing. -->
        <p class="body">
          <template v-if="voice.transcript.segments?.length">
            <span
              v-for="(segment, i) in voice.transcript.segments"
              :key="i"
              :class="{ active: i === activeSegment }"
              >{{ segment.text }}{{
                i < voice.transcript.segments.length - 1 ? ' ' : ''
              }}</span
            >
          </template>
          <template v-else>{{ voice.transcript.text }}</template>
        </p>
        <div class="foot">
          <button class="action plain">COPY</button>
          <button class="action plain">REPORT BAD TRANSCRIPT</button>
          <span class="engine"
            >{{ (voice.transcript.engine ?? '').toUpperCase() }} ·
            {{ (voice.transcript.language ?? '').toUpperCase() }}</span
          >
        </div>
      </template>

      <!-- B · READY, COLLAPSED — default ──────────────────────────── -->
      <template v-else>
        <div class="strip">
          <span class="label">TRANSCRIPT</span>
          <span class="rule" />
          <!-- Word count instead of a chevron: tells you whether this is a
               sentence or a monologue before you open it. -->
          <button class="action" @click="toggleTranscript(message.id)">
            EXPAND · {{ words }} W
          </button>
        </div>
        <p class="body clamp">{{ voice.transcript.text }}</p>
      </template>
    </div>
  </div>
</template>

<style scoped>
.voice {
  border: 1px solid var(--line-edge);
  background: var(--surface-lift);
}

.player {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 14px;
}

.play {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: center;
  width: 30px;
  height: 30px;
  font-size: 11px;
  color: var(--on-accent);
  background: var(--accent-wire);
}

.time {
  flex: none;
  font-size: 11px;
  color: var(--text-secondary);
}
.time.live {
  color: var(--text-primary);
}

.rate {
  flex: none;
  padding: 2px 5px;
  font: var(--type-label);
  font-size: 10px;
  letter-spacing: 0;
  color: var(--text-meta);
  border: 1px solid var(--line-edge);
}
.rate.on {
  color: var(--accent-wire);
  border-color: var(--line-accent-dim);
}

.transcript {
  padding: 10px 14px 12px;
  border-top: 1px solid var(--line-inner);
}

.strip {
  display: flex;
  align-items: center;
  gap: 10px;
}

.strip .rule {
  flex: 1;
  height: 1px;
  background: var(--line-inner);
}

.label {
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: var(--track-label);
  color: var(--text-meta);
}

.action {
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.1em;
  color: var(--accent-wire);
}
.action.plain {
  letter-spacing: 0.12em;
  color: var(--text-meta);
}

.pending-label {
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: var(--track-label);
  color: var(--accent-dim);
}

.dot {
  display: block;
  flex: none;
  width: 5px;
  height: 5px;
  background: var(--accent-wire);
}
.pulse {
  animation: catPulse 1.3s ease-in-out infinite;
}

.skeleton {
  margin-top: 7px;
}
.skeleton .line {
  display: flex;
  align-items: center;
  height: 20px; /* one transcript line — the reservation is the point */
}
.skeleton i {
  display: block;
  height: 9px;
  background: var(--surface-raised);
  animation: catShimmer 1.6s ease-in-out infinite;
}

/* Styled as secondary throughout: present, but clearly machine output, so it
 * never competes with what someone actually typed. */
.body {
  margin: 7px 0 0;
  max-width: 70ch;
  font: var(--type-secondary);
  color: var(--text-secondary);
}

.body.clamp {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.body .active {
  color: var(--text-primary);
  background: var(--accent-wash);
  box-shadow: inset 0 -1px 0 var(--accent-underline);
}

.foot {
  display: flex;
  gap: 18px;
  align-items: center;
  margin-top: 11px;
  padding-top: 10px;
  border-top: 1px solid var(--line-inner);
}

.engine {
  margin-left: auto;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.12em;
  color: var(--text-dim);
}
</style>
