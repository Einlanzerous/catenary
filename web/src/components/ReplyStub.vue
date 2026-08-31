<script setup lang="ts">
import { computed } from 'vue'
import type { ReplyRef } from '@/types'
import { duration } from '@/lib/format'
import { jumpTo, messageById, user, voiceOf } from '@/store'

/**
 * One line, always. Never wraps, never grows — ten replies in a row cost ten
 * identical 17px lines, so the thread's rhythm survives. No nesting: a reply
 * to a reply quotes only its immediate parent.
 */
const props = defineProps<{ reply: ReplyRef; armed?: boolean }>()

const author = computed(() => user(props.reply.authorId))

/** Read from the live source, not from a copy taken at send time, so a stub
 *  back-fills in place when its transcript lands. */
const source = computed(() => messageById(props.reply.messageId))
const sourceVoice = computed(() => voiceOf(source.value))

const pending = computed(
  () => props.reply.kind === 'voice' && sourceVoice.value?.transcript.state === 'pending',
)

const preview = computed(() => {
  if (pending.value) return 'transcript pending — no preview text yet'
  if (props.reply.kind === 'voice') {
    return sourceVoice.value?.transcript.text ?? props.reply.preview
  }
  if (props.reply.kind === 'image') return `photo · ${props.reply.preview}`
  return props.reply.preview
})
</script>

<template>
  <!-- Whole stub is the target, not just the text. -->
  <component
    :is="armed ? 'div' : 'button'"
    class="stub"
    :class="{ armed }"
    @click="!armed && jumpTo(reply.messageId)"
  >
    <span class="rule" />
    <span class="line">
      <span v-if="armed" class="chip accent">REPLYING TO</span>
      <span class="sender">{{ author?.name.toUpperCase() }}</span>

      <!-- Non-text sources get a typed mono chip; the chip is the accent's
           only appearance in the stub. -->
      <span
        v-if="reply.kind === 'image'"
        class="thumb"
        aria-hidden="true"
      />
      <span
        v-else-if="reply.kind === 'voice'"
        class="chip"
        :class="pending ? 'dim' : 'accent'"
        >▶ VOICE {{ duration(reply.durationSec ?? 0) }}</span
      >
      <span v-else-if="reply.kind === 'link'" class="chip accent">↗ LINK</span>

      <span class="preview" :class="{ pending }">{{ preview }}</span>
    </span>
  </component>
</template>

<style scoped>
.stub {
  display: flex;
  gap: 10px;
  width: 100%;
  max-width: var(--measure);
  margin-top: 5px;
  text-align: left;
}

.stub:not(.armed) {
  cursor: pointer;
}

.rule {
  flex: none;
  width: 2px;
  align-self: stretch;
  background: var(--surface-avatar);
}

.armed .rule {
  background: var(--accent-wire);
}

.line {
  display: flex;
  min-width: 0;
  gap: 8px;
  align-items: baseline;
  overflow: hidden;
}

.sender {
  flex: none;
  font: var(--type-label);
  font-size: 10px;
  letter-spacing: 0.06em;
  color: var(--text-quote);
}

.chip {
  flex: none;
  font: var(--type-label);
  font-size: 10px;
  letter-spacing: 0.06em;
}
.chip.accent {
  color: var(--accent-wire);
}
.chip.dim {
  color: var(--accent-dim);
}

.thumb {
  flex: none;
  align-self: center;
  width: 18px;
  height: 18px;
  background-image: repeating-linear-gradient(
    135deg,
    var(--line-edge) 0 4px,
    var(--line-inner) 4px 8px
  );
}

.preview {
  overflow: hidden;
  font-size: 12.5px;
  line-height: 17px;
  color: var(--text-meta);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.preview.pending {
  color: var(--text-placeholder);
}
</style>
