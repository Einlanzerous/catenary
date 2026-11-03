<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import type { ReplyRef } from '@/wire/generated'
import { duration } from '@/lib/format'
import { peaksFromSeed } from '@/lib/waveform'
import { IMAGE_ACCEPT, acceptedLine, dragCarriesFiles } from '@/attach'
import { hostOf, plainText } from '@/linkpreview'
import {
  attachImage,
  canAttachFile,
  canRecord,
  closeAttachZone,
  composerDraftChanged,
  dismissPreview,
  messageById,
  pasteIntoComposer,
  removeAttachment,
  replyTo,
  send,
  startRecording,
  state,
  stopRecording,
  toggleAttachZone,
  transcriptText,
  voiceOf,
} from '@/store'
import HeldChip from './HeldChip.vue'
import ReplyStub from './ReplyStub.vue'
import Waveform from './Waveform.vue'

const props = defineProps<{ conversationName: string }>()

const box = ref<HTMLTextAreaElement | null>(null)
const picker = ref<HTMLInputElement | null>(null)
const attachButton = ref<HTMLButtonElement | null>(null)
const chooseButton = ref<HTMLButtonElement | null>(null)

/** Focus lands on CHOOSE FILE when the zone opens; closing from inside it
 *  (ESC) hands focus back to ATTACH. A refusal moves nothing. */
watch(
  () => state.composer.attachOpen,
  (open) => {
    if (open) void nextTick(() => chooseButton.value?.focus())
  },
)
/** The outbox stopped taking files (ATTACH is now dimmed): the zone goes. */
watch(canAttachFile, (can) => !can && closeAttachZone())

function closeZoneToAttach() {
  closeAttachZone()
  void nextTick(() => attachButton.value?.focus())
}

const acceptLine = acceptedLine()
/** The zone's own dragover state (CANT-354): only a drag carrying files. */
const over = ref(false)
function onZoneDrag(event: DragEvent) {
  if (!dragCarriesFiles(event.dataTransfer?.types)) return
  event.preventDefault()
  over.value = true
}
function onZoneDragLeave(event: DragEvent) {
  if (!(event.currentTarget as HTMLElement).contains(event.relatedTarget as Node | null)) over.value = false
}
/** A drop onto the zone goes through the store's one gate, like the chooser. */
function onZoneDrop(event: DragEvent) {
  over.value = false
  if (!dragCarriesFiles(event.dataTransfer?.types)) return
  event.preventDefault()
  void attachImage([...(event.dataTransfer?.files ?? [])], 'drop')
}
/** The refusal line: the message, and the file facts only if they are for it. */
const refusal = computed(() => {
  const r = state.composer.attachRefusal
  return r && r.message === state.composer.attachError ? r : null
})

/** The file dialog's result, through the store's one gate. The input is not
 *  `multiple`, and the store replaces whatever was held. It is cleared so
 *  choosing the same file again still fires; the files are copied first, since
 *  a FileList is live. */
async function onPick(event: Event) {
  const input = event.target as HTMLInputElement
  const files = [...(input.files ?? [])]
  input.value = ''
  await attachImage(files, 'chooser')
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
    return
  }
  if (event.key === 'Escape' && state.composer.attachOpen) {
    event.preventDefault()
    closeZoneToAttach()
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
        <!-- ATT-02: the drop zone, the FIRST row inside the field. Not a modal.
             Refusals stay in it as one line; the file name is untrusted and is
             only ever interpolated as text. -->
        <!-- CANT-355: the zone owns its drags (`.stop`), so the thread's overlay
             never draws over it and a file dropped here is attached once. -->
        <div
          v-if="state.composer.attachOpen"
          id="attach-zone"
          class="zone"
          :class="{ over }"
          role="region"
          aria-label="Attach an image"
          @keydown.esc.stop.prevent="closeZoneToAttach"
          @dragenter.stop="onZoneDrag"
          @dragover.stop="onZoneDrag"
          @dragleave.stop="onZoneDragLeave"
          @drop.stop="onZoneDrop"
        >
          <div class="zone-head">
            <span class="zone-prompt">{{ over ? 'Release to attach.' : 'Drop an image here, or paste one.' }}</span>
            <span class="zone-esc">ESC CLOSES</span>
          </div>
          <button ref="chooseButton" type="button" class="choose" @click="picker?.click()">CHOOSE FILE</button>
          <span class="zone-accept">{{ acceptLine }}</span>
          <div class="zone-status" role="status" aria-live="polite">
            <div v-if="state.composer.attachError" class="zone-refusal">
              <div v-if="refusal && (refusal.name || refusal.tag)" class="zone-file">
                <span class="zone-name">{{ plainText(refusal.name) }}</span>
                <span v-if="refusal.tag" class="zone-tag tnum">{{ refusal.tag }}</span>
              </div>
              <div class="zone-message">{{ state.composer.attachError }}</div>
            </div>
          </div>
        </div>

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
          @paste="pasteIntoComposer"
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
            ref="attachButton"
            class="ghost"
            :class="{ off: !canAttachFile, open: state.composer.attachOpen }"
            :disabled="!canAttachFile"
            :aria-expanded="state.composer.attachOpen"
            aria-controls="attach-zone"
            @click="toggleAttachZone"
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
      <p v-if="state.composer.attachError && !state.composer.attachOpen" class="hint attach-error" role="status">
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
/* ATTACH while its zone is open (CANT-357 ruling 3): text.primary and a 1px
 * text.primary underline, drawn inside the button so no row moves. */
.ghost.open {
  padding-bottom: 2px;
  color: var(--text-primary);
  box-shadow: inset 0 -1px 0 var(--text-primary);
}

/* The drop zone (canvas 08, ATT-02): surface.rail ground, 1px line.hair, inset
 * 8px like the chips and never wider than the field. Colors are tokens by name,
 * and state swaps are instant. */
.zone {
  display: grid;
  justify-items: start;
  margin: 8px 8px 0;
  padding: 14px 16px;
  background: var(--surface-rail);
  border: 1px solid var(--line-hair);
}
.zone.over {
  background: var(--surface-raised);
  border-color: var(--text-meta);
}
.zone > * {
  min-width: 0;
  margin-top: 12px;
}
.zone > :first-child {
  margin-top: 0;
}
.zone-head {
  display: flex;
  gap: 16px;
  align-items: baseline;
  justify-content: space-between;
  width: 100%;
}
.zone-prompt {
  font-size: 13.5px;
  line-height: 20px;
  color: var(--text-primary);
}
.zone-esc {
  flex: none;
  font-family: var(--font-mono);
  font-size: 10px;
  letter-spacing: 0.12em;
  color: var(--text-secondary);
}
.choose {
  display: inline-flex;
  align-items: center;
  height: 32px;
  padding: 0 14px;
  font-family: var(--font-mono);
  font-size: 10px;
  letter-spacing: 0.14em;
  color: var(--text-primary);
  background: var(--surface-base);
  border: 1px solid var(--line-hair);
  transition:
    background-color 80ms linear,
    border-color 80ms linear;
}
.choose:hover {
  background: var(--surface-raised);
}
.choose:focus-visible {
  outline: 2px solid var(--text-primary);
  outline-offset: 2px;
}
.choose:active {
  background: var(--surface-raised);
  border-color: var(--text-meta);
}
.zone-accept {
  font-family: var(--font-mono);
  font-size: 10.5px;
  line-height: 14px;
  color: var(--text-secondary);
}
/* The status region is always there so a screen reader hears the line arrive;
 * empty, it takes no room and adds no gap. */
.zone > .zone-status {
  width: 100%;
  margin-top: 0;
}
.zone-refusal {
  display: grid;
  gap: 3px;
  margin-top: 12px;
  padding-top: 10px;
  border-top: 1px solid var(--line-hair);
}
.zone-file {
  display: flex;
  gap: 10px;
  min-width: 0;
  font-family: var(--font-mono);
  font-size: 10.5px;
  line-height: 14px;
  color: var(--text-secondary);
}
.zone-name {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.zone-tag {
  flex: none;
}
.zone-message {
  font-size: 13.5px;
  line-height: 20px;
  color: var(--text-primary);
  text-wrap: pretty;
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
  .choose {
    height: 44px;
  }
}
</style>
