<script setup lang="ts">
import { computed } from 'vue'
import type { ImageAttachment, Message } from '@/types'
import { clock } from '@/lib/format'
import { discard, isMine, jumpTo, replyTo, retry, state, user, voiceOf } from '@/store'
import Avatar from './Avatar.vue'
import ImageAttachmentView from './ImageAttachment.vue'
import ReplyStub from './ReplyStub.vue'
import StatusLabel from './StatusLabel.vue'
import VoiceNote from './VoiceNote.vue'

const props = defineProps<{
  message: Message
  previous?: Message
  memberCount: number
}>()

const mine = computed(() => isMine(props.message))
const author = computed(() => user(props.message.authorId))
const voice = computed(() => voiceOf(props.message))
const images = computed<ImageAttachment[]>(
  () =>
    props.message.attachments?.filter(
      (a): a is ImageAttachment => a.kind === 'image',
    ) ?? [],
)

/** A new author, or a reply, starts a group and gets the name header. */
const startsGroup = computed(
  () =>
    !props.previous ||
    props.previous.authorId !== props.message.authorId ||
    !!props.message.replyTo,
)

const failed = computed(() => props.message.state === 'failed')
const arrived = computed(() => state.arrivedAt === props.message.id)

/** Jumping backward in a long thread should never cost you your place. */
const returnTo = computed(() => {
  if (!arrived.value) return null
  const last = state.messages
    .filter((m) => m.conversationId === props.message.conversationId)
    .sort((a, b) => a.seq - b.seq)
    .at(-1)
  return last && last.id !== props.message.id ? last : null
})
</script>

<template>
  <div
    class="row"
    :class="{
      mine,
      failed,
      arrived,
      group: startsGroup,
      sending: message.state === 'sending',
      muted: message.state === 'queued',
    }"
  >
    <!-- Deliberate call 02: a timestamp on every line, including
         continuations — never hover-reveal, because hover-only data is
         invisible in screenshots and on touch. -->
    <div class="gutter meta tnum" :class="{ faint: !startsGroup }">
      {{ clock(message.at) }}
    </div>

    <div class="body">
      <div v-if="startsGroup" class="head">
        <Avatar :user-id="message.authorId" :size="20" />
        <span class="name">{{ mine ? 'You' : author?.name }}</span>
        <!-- Narrow only: the gutter collapses and the stamp comes inline,
             still tabular. -->
        <span class="inline-time meta tnum">{{ clock(message.at) }}</span>
        <button v-if="returnTo" class="back" @click="jumpTo(returnTo.id)">
          ↓ BACK TO {{ clock(returnTo.at) }}
        </button>
        <button class="reply-action" @click="replyTo(message.id)">REPLY</button>
      </div>

      <ReplyStub v-if="message.replyTo" :reply="message.replyTo" />

      <p v-if="message.text" class="text measure">{{ message.text }}</p>

      <VoiceNote
        v-if="voice"
        class="attachment"
        :message="message"
        :voice="voice"
      />

      <div v-if="images.length" class="images">
        <ImageAttachmentView
          v-for="(image, i) in images"
          :key="i"
          :image="image"
        />
      </div>

      <!-- Failure is a row, not a toast: toasts expire, failures don't. -->
      <div v-if="failed" class="fault">
        <span class="fault-text">{{ message.error }}</span>
        <button class="retry" @click="retry(message.id)">RETRY</button>
        <button class="delete" @click="discard(message.id)">DELETE</button>
      </div>
    </div>

    <div class="status">
      <StatusLabel
        v-if="mine"
        :message="message"
        :member-count="memberCount"
      />
    </div>
  </div>
</template>

<style scoped>
.row {
  display: grid;
  grid-template-columns: var(--gutter-w) 1fr var(--status-w);
  column-gap: var(--s4);
  padding: var(--row-y) var(--s6) var(--row-y) 0;
}

.row.group {
  margin-top: var(--group-gap);
  padding-top: 2px;
  padding-bottom: 2px;
}

/* Deliberate call 01: no bubbles. Own messages get a one-step surface lift
 * and an accent "You", so a long thread reads as a document. */
.row.mine {
  background: var(--surface-lift);
}

.row.failed {
  background: var(--surface-lift);
  border-left: 2px solid var(--signal-fault);
  padding-left: 0;
}

.row.arrived {
  border-left: 2px solid var(--accent-wire);
  animation: catArrival 1.6s ease-out forwards;
}

.row.muted {
  opacity: 0.62;
}

.gutter {
  text-align: right;
  font-size: 10.5px;
  color: var(--text-dim);
  padding-top: 4px;
}

.row.group .gutter {
  padding-top: 23px; /* aligns with the first line of body text, not the name */
}

.gutter.faint {
  color: var(--text-faint);
}

.head {
  display: flex;
  align-items: center;
  gap: 9px;
}

.name {
  font: var(--type-sender);
}

.inline-time {
  display: none;
  font-size: 10.5px;
  color: var(--text-dim);
}

.row.mine .name {
  color: var(--accent-wire);
}

.back {
  margin-left: auto;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.1em;
  color: var(--accent-wire);
}

.reply-action {
  margin-left: auto;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.12em;
  color: var(--text-meta);
  opacity: 0;
}
.back ~ .reply-action {
  margin-left: 12px;
}
.row:hover .reply-action,
.reply-action:focus-visible {
  opacity: 1;
}

.text {
  margin: 3px 0 0;
  font: var(--type-body);
  color: var(--text-primary);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.row.sending .text {
  opacity: 0.62;
}

.attachment {
  margin-top: var(--s2);
}

.images {
  display: flex;
  gap: var(--s2);
  margin-top: var(--s2);
  padding-bottom: 3px;
}

.fault {
  display: flex;
  gap: 14px;
  align-items: center;
  margin-top: 4px;
}

.fault-text {
  font: var(--type-meta);
  font-size: 10.5px;
  color: var(--signal-fault);
}

.retry {
  padding: 3px 9px;
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--on-accent);
  background: var(--signal-fault);
}

.delete {
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--text-secondary);
}

.status {
  padding-top: 8px;
}

.row.group .status {
  padding-top: 27px;
}

/* Below 900px the 72px time gutter collapses and the timestamp moves inline
 * after the sender name, still tabular. */
@media (max-width: 900px) {
  .row {
    grid-template-columns: 1fr;
    column-gap: 0;
    padding: var(--row-y) var(--s4);
  }
  .gutter {
    display: none;
  }
  .inline-time {
    display: inline;
  }
  .status {
    padding-top: 2px;
    text-align: left;
  }
  .row.group .status {
    padding-top: 2px;
  }
}
</style>
