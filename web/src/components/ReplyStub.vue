<script setup lang="ts">
import { computed } from 'vue'
import type { ReplyRef } from '@/wire/generated'
import { duration } from '@/lib/format'
import { hostOf, plainText } from '@/linkpreview'
import { isSilent, jumpTo, messageById, NO_SPEECH_LABEL, REPLY_NO_TRANSCRIPT_LABEL, REPLY_VOICE_NOTE_LABEL, transcriptText, user, voiceOf } from '@/store'

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

/** A reply to a card quotes the card (canvas 08, section 3): `↗ LINK`, the
 *  host, then the title; an empty preview ends at the host. Read from the live
 *  source. A source that is not held falls back to what the ref itself says. */
const card = computed(() => {
  if (props.reply.kind !== 'link') return null
  const a = source.value?.attachments?.find((x) => x.kind === 'link')
  return a?.kind === 'link' ? { host: hostOf(a.url), title: plainText(a.title) } : null
})

const preview = computed(() => {
  if (pending.value) return 'transcript pending — no preview text yet'
  if (props.reply.kind === 'voice') {
    // A loaded source whose job finished with no words (CANT-236 ruling 0, A).
    // The server's preview for it is '' too, so this has to be a literal here.
    if (isSilent(sourceVoice.value)) return NO_SPEECH_LABEL.toLowerCase()
    // Never an empty line (CANT-283). Text that exists is drawn as sent; if
    // there is none, say only what the stub can derive from the state it holds.
    const text = transcriptText(sourceVoice.value) ?? props.reply.preview
    if (text.trim()) return text
    if (sourceVoice.value?.transcript.state === 'failed') return REPLY_NO_TRANSCRIPT_LABEL
    return REPLY_VOICE_NOTE_LABEL
  }
  if (props.reply.kind === 'image') return `photo · ${props.reply.preview}`
  // A link reply's preview is the source's own text, and is empty for a
  // message that is only a card. Never an empty line: name what the stub can
  // derive from the source it holds, the host of the card's URL, else nothing
  // more than "link".
  if (props.reply.kind === 'link' && !props.reply.preview.trim()) {
    return 'link'
  }
  return props.reply.preview
})
</script>

<template>
  <!-- Whole stub is the target, not just the text. -->
  <component
    :is="armed ? 'div' : 'button'"
    class="stub"
    :class="{ armed, link: reply.kind === 'link' }"
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
        >▶ VOICE {{ duration(reply.durationMs ?? 0) }}</span
      >
      <span v-else-if="reply.kind === 'link'" class="chip meta">↗ LINK</span>

      <template v-if="card">
        <span v-if="card.host" class="host">{{ card.host }}</span>
        <span v-if="card.title" class="preview">{{ card.title }}</span>
      </template>
      <span v-else class="preview" :class="{ pending }">{{ preview }}</span>
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
.chip.meta {
  color: var(--text-meta);
}
.chip.dim {
  color: var(--accent-dim);
}

/* A link stub is drawn in the canvas's tokens (08, section 3): the rule is
 * line.hair, the sender text.secondary, and `↗ LINK` text.meta, not copper. */
.stub.link .rule {
  background: var(--line-hair);
}
.stub.link.armed .rule {
  background: var(--accent-wire);
}
.stub.link .sender {
  color: var(--text-secondary);
}

.host {
  flex: 0 1 auto;
  min-width: 0;
  max-width: 45%;
  overflow: hidden;
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--text-secondary);
  text-overflow: ellipsis;
  white-space: nowrap;
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
