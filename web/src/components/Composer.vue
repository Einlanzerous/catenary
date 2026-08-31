<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { ReplyRef } from '@/types'
import { duration } from '@/lib/format'
import { peaksFromSeed } from '@/lib/waveform'
import {
  messageById,
  replyTo,
  send,
  startRecording,
  state,
  stopRecording,
  voiceOf,
} from '@/store'
import ReplyStub from './ReplyStub.vue'
import Waveform from './Waveform.vue'

const props = defineProps<{ conversationName: string }>()

const box = ref<HTMLTextAreaElement | null>(null)

/** The app never pretends a message left the building. */
const offline = computed(() => state.connection.state !== 'live')
/** The armed bar quotes the message being replied *to* — not that message's
 *  own reply, which is what a naive `message.replyTo` would show. */
const armed = computed<ReplyRef | null>(() => {
  const source = messageById(state.composer.replyToId ?? '')
  if (!source) return null

  const voice = voiceOf(source)
  if (voice) {
    return {
      messageId: source.id,
      authorId: source.authorId,
      kind: 'voice',
      durationSec: voice.durationSec,
      preview: voice.transcript.text ?? '',
    }
  }

  const image = source.attachments?.find((a) => a.kind === 'image')
  if (image && image.kind === 'image') {
    return {
      messageId: source.id,
      authorId: source.authorId,
      kind: 'image',
      preview: image.filename,
    }
  }

  return {
    messageId: source.id,
    authorId: source.authorId,
    kind: 'text',
    preview: source.text ?? '',
  }
})

const livePeaks = peaksFromSeed(2281, 70)

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Enter' && !event.shiftKey) {
    event.preventDefault()
    send()
    return
  }
  if (event.key === 'Escape' && state.composer.replyToId) {
    event.preventDefault()
    replyTo(null)
  }
}

function onRecordKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') stopRecording(false)
}

// Arming a reply moves focus into the box — the reply is the action, the
// header is only the receipt.
watch(armed, (value) => value && box.value?.focus())

const grow = () => {
  const el = box.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(el.scrollHeight, 220)}px`
}
</script>

<template>
  <div class="composer" @keydown="onRecordKeydown">
    <!-- A · RECORDING. Amber, not red: red belongs to failure alone. -->
    <div v-if="state.composer.recording" class="record">
      <span class="dot" />
      <span class="clock tnum">{{ duration(state.composer.recordingSec) }}</span>
      <Waveform :peaks="livePeaks" :played="1" :bars="70" :height="26" />
      <button class="ghost" @click="stopRecording(false)">CANCEL</button>
      <button class="primary" @click="stopRecording(true)">SEND</button>
    </div>
    <p v-if="state.composer.recording" class="hint">
      ESC cancels · SPACE pauses · release-to-send is off by default
    </p>

    <template v-else>
      <div class="field">
        <!-- B · REPLY ARMED -->
        <div v-if="armed" class="armed-bar">
          <ReplyStub :reply="armed" armed />
          <button class="dismiss" aria-label="Clear reply" @click="replyTo(null)">
            ✕
          </button>
        </div>

        <textarea
          ref="box"
          v-model="state.composer.draft"
          class="input"
          rows="1"
          :placeholder="
            offline
              ? `Message ${props.conversationName} — will send when reconnected`
              : `Message ${props.conversationName}`
          "
          @input="grow"
          @keydown="onKeydown"
        />

        <div class="actions">
          <!-- Uploads can't be queued safely, so ATTACH dims when offline. -->
          <button class="ghost" :class="{ off: offline }" :disabled="offline">
            ATTACH
          </button>
          <button class="ghost" @click="startRecording">RECORD</button>
          <button class="ghost hide-narrow">MARKDOWN</button>
          <span class="keys">
            {{ armed ? 'ESC CLEARS REPLY' : '⏎ SEND · ⇧⏎ NEWLINE' }}
          </span>
          <button
            class="primary"
            :class="{ queue: offline }"
            @click="send"
          >
            {{ offline ? 'QUEUE' : 'SEND' }}
          </button>
        </div>
      </div>
      <!-- The typing indicator used to sit here as a strip. Call 10a moved it
           into the thread as its own last row, so the composer stays the
           bottom of the window and nothing shifts under the cursor. -->
    </template>
  </div>
</template>

<style scoped>
.composer {
  padding: 12px var(--s6) 14px;
  background: var(--surface-rail);
  border-top: 1px solid var(--line-hair);
}

.field {
  background: var(--surface-base);
  border: 1px solid var(--line-edge);
  border-radius: var(--radius-input);
}

.armed-bar {
  display: flex;
  gap: 10px;
  align-items: center;
  padding: 9px 12px 9px 14px;
  background: var(--surface-lift);
  border-bottom: 1px solid var(--line-inner);
}

.armed-bar > :first-child {
  flex: 1;
  min-width: 0;
  margin-top: 0;
}

.dismiss {
  flex: none;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-secondary);
}

.input {
  display: block;
  width: 100%;
  padding: 11px 14px 4px;
  font: var(--type-body);
  line-height: 24px;
  color: var(--text-primary);
  background: none;
  border: 0;
  resize: none;
  outline: none;
}

.input::placeholder {
  color: var(--text-secondary);
}

.actions {
  display: flex;
  gap: var(--s4);
  align-items: center;
  padding: 6px 12px 9px 14px;
}

.ghost {
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--text-meta);
}
.ghost.off {
  color: var(--text-disabled);
  cursor: not-allowed;
}

.keys {
  margin-left: auto;
  font: var(--type-label);
  letter-spacing: 0.1em;
  color: var(--text-dim);
}

.primary {
  padding: 5px 12px;
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--on-accent);
  background: var(--accent-wire);
}
.primary.queue {
  background: var(--accent-queue);
}

.record {
  display: flex;
  gap: 14px;
  align-items: center;
  padding: 10px 12px;
  background: var(--surface-base);
  border: 1px solid var(--accent-wire);
  border-radius: var(--radius-input);
}

.clock {
  font-family: var(--font-mono);
  font-size: 13px;
  color: var(--text-primary);
}

.dot {
  display: block;
  flex: none;
  width: 8px;
  height: 8px;
  background: var(--accent-wire);
  animation: catPulse 1.4s ease-in-out infinite;
}
.hint {
  display: flex;
  gap: var(--s2);
  align-items: center;
  margin: var(--s2) 0 0;
  font: var(--type-label);
  font-size: 10px;
  letter-spacing: 0;
  color: var(--text-meta);
}

@media (max-width: 900px) {
  .composer {
    padding: 10px var(--s4) 12px;
  }
  .hide-narrow {
    display: none;
  }
  .keys {
    display: none;
  }
}
</style>
