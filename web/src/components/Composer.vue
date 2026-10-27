<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { ReplyRef } from '@/wire/generated'
import { duration } from '@/lib/format'
import { peaksFromSeed } from '@/lib/waveform'
import { IMAGE_ACCEPT } from '@/attach'
import { hostOf, plainText } from '@/linkpreview'
import {
  canAttachFile,
  canRecord,
  composerDraftChanged,
  dismissPreview,
  messageById,
  pickImage,
  removeAttachment,
  replyTo,
  send,
  startRecording,
  state,
  stopRecording,
  transcriptText,
  voiceOf,
} from '@/store'
import HeldChip from './HeldChip.vue'
import ReplyStub from './ReplyStub.vue'
import Waveform from './Waveform.vue'

const props = defineProps<{ conversationName: string }>()

const box = ref<HTMLTextAreaElement | null>(null)
const picker = ref<HTMLInputElement | null>(null)

/** The file dialog's result. One image: the input is not `multiple`, and the
 *  store replaces whatever was held. The input is cleared so choosing the same
 *  file again still fires. */
async function onPick(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (file) await pickImage(file)
}

/** The app never pretends a message left the building. ATTACH does not use
 *  this: its enablement is the outbox's own predicate (`canAttachFile`). */
const offline = computed(() => state.connection.state !== 'live')
/** CANT-31 §6: nothing composed now will drain until a relaunch or a
 *  re-enrollment, so the composer does not offer to queue it (Invariant 3). */
const terminal = computed(() => state.connection.state === 'terminal')
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
      durationMs: voice.durationMs,
      preview: transcriptText(voice) ?? '',
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

  if (source.attachments?.some((a) => a.kind === 'link')) {
    return { messageId: source.id, authorId: source.authorId, kind: 'link', preview: source.text ?? '' }
  }

  return {
    messageId: source.id,
    authorId: source.authorId,
    kind: 'text',
    preview: source.text ?? '',
  }
})

/** The held link card: shown while it is asked for and once it is ready, and
 *  nowhere else. No card, a refused ask and a dismissed one all show nothing,
 *  and none says why. The host is derived from the URL the sender typed. */
const previewShown = computed(() => state.composer.preview.state === 'ready' || state.composer.preview.state === 'loading')
const fetching = computed(() => state.composer.preview.state === 'loading')
const previewHost = computed(() => hostOf(state.composer.preview.card?.url ?? state.composer.preview.url ?? ''))

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

        <!-- What SEND will send, held above the text (canvas 08, section 1):
             the link being fetched or fetched, then the picked image, in one
             tag column. Page-chosen words render as text. An empty or failed
             fetch shows nothing here at all, the same exit as the cross. -->
        <div v-if="previewShown || state.composer.attachment" class="chips">
          <HeldChip
            v-if="previewShown"
            tag="LINK"
            :name="previewHost"
            :title="plainText(state.composer.preview.card?.title)"
            :fetching="state.composer.preview.state === 'loading'"
            :dismiss-label="state.composer.preview.state === 'loading' ? 'Skip link preview' : 'Remove link preview'"
            @dismiss="dismissPreview"
          />
          <HeldChip
            v-if="state.composer.attachment"
            tag="IMAGE"
            :name="plainText(state.composer.attachment.filename) || 'image'"
            dismiss-label="Remove attachment"
            @dismiss="removeAttachment"
          />
        </div>

        <textarea
          ref="box"
          v-model="state.composer.draft"
          class="input"
          :class="{ held: previewShown || state.composer.attachment }"
          rows="1"
          :placeholder="
            terminal
              ? `Message ${props.conversationName} — this device cannot send; see the banner`
              : offline
                ? `Message ${props.conversationName} — will send when reconnected`
                : `Message ${props.conversationName}`
          "
          @input="grow(); composerDraftChanged()"
          @keydown="onKeydown"
        />

        <div class="actions">
          <!-- Uploads can't be queued safely, so ATTACH is enabled exactly when
               the outbox would take a picked file now. One image, no
               `multiple`; the dialog's `accept` is a hint, the check is the
               store's. -->
          <input
            ref="picker"
            class="picker"
            type="file"
            :accept="IMAGE_ACCEPT"
            tabindex="-1"
            hidden
            @change="onPick"
          />
          <button
            class="ghost"
            :class="{ off: !canAttachFile }"
            :disabled="!canAttachFile"
            @click="picker?.click()"
          >
            ATTACH
          </button>
          <!-- REC is drawn disabled until CANT-56 makes a voice message
               sendable (CANT-247 ruling 0 → B): `canRecord` is the one gate.
               Offline it stays allowed; only a terminal client dims it. -->
          <button
            class="ghost"
            :class="{ off: terminal || !canRecord }"
            :disabled="terminal || !canRecord"
            @click="startRecording"
          >
            RECORD
          </button>
          <button class="ghost hide-narrow">MARKDOWN</button>
          <span class="keys">
            {{ fetching ? 'SENDING NOW SENDS WITHOUT A CARD' : armed ? 'ESC CLEARS REPLY' : '⏎ SEND · ⇧⏎ NEWLINE' }}
          </span>
          <button
            class="primary"
            :class="{ queue: offline && !terminal }"
            :disabled="terminal"
            @click="send"
          >
            {{ offline && !terminal ? 'QUEUE' : 'SEND' }}
          </button>
        </div>
      </div>
      <p v-if="state.composer.attachError" class="hint attach-error" role="status">
        {{ state.composer.attachError }}
      </p>
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
  background: var(--surface-rail);
  border-bottom: 1px solid var(--line-hair);
}

.armed-bar > :first-child {
  flex: 1;
  min-width: 0;
  max-width: none;
  margin-top: 0;
}

.chips {
  display: flex;
  flex-direction: column;
  gap: 6px;
  align-items: flex-start;
  padding: 8px 8px 0;
}

.dismiss {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-secondary);
}
.dismiss:hover {
  color: var(--text-primary);
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

/* Under a held chip the text sits 2px closer (canvas 1A: 9px, not 11px). */
.input.held {
  padding-top: 9px;
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
/* Terminal: nothing sends, so the one accent does not sit on this button. */
.primary:disabled {
  color: var(--text-disabled);
  background: var(--surface-base);
  cursor: not-allowed;
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
  .dismiss {
    width: 44px;
    height: 44px;
    font-size: 12px;
  }
  .chips {
    padding: 8px 8px 0;
    align-items: stretch;
  }
  .keys {
    display: none;
  }
}
</style>
