<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { ImageAttachment } from '@/client-types'
import { decodeBlurhash } from '@/lib/blurhash'
import { megabytes } from '@/lib/format'
import { imageBox, thumbUrl } from '@/lib/image-box'

const props = defineProps<{ image: ImageAttachment }>()

/**
 * CANT-49. The box is computed from the *stored* dimensions and nothing else,
 * so the row height is final before a byte of the image arrives: no reflow,
 * no jumping scroll position mid-read. The blurhash fills the box first, the
 * thumbnail (`&v=thumb`, CANT-47) fades in over it, and the lightbox asks for
 * the plain URL. `loaded`, `failed` and `open` change what is painted inside
 * the box and never the box itself (web/image-box.test.ts holds that).
 */
const box = computed(() => imageBox(props.image.width, props.image.height))
const frameStyle = computed(() => ({ height: `${box.value.h}px` }))
const figureStyle = computed(() => ({ width: `${box.value.w}px` }))

// An outbox entry has no media URL until the upload exists.
const hasUrl = computed(() => props.image.url !== '')
const inline = computed(() => thumbUrl(props.image.url))
const uploading = computed(() => props.image.uploadedBytes !== undefined)
const percent = computed(() =>
  props.image.bytes > 0
    ? Math.min(100, Math.round(((props.image.uploadedBytes ?? 0) / props.image.bytes) * 100))
    : 0,
)

const loaded = ref(false)
const failed = ref(false)
const pic = ref<HTMLImageElement | null>(null)
const blur = ref<HTMLCanvasElement | null>(null)

// Blurhash at 32 px on the long side, scaled to the box by CSS. Absent or
// malformed means the plane stays flat; it never blocks the image.
function paint() {
  const el = blur.value
  if (!el || !props.image.placeholder) return
  const w = box.value.w >= box.value.h ? 32 : Math.max(1, Math.round((32 * box.value.w) / box.value.h))
  const h = box.value.w >= box.value.h ? Math.max(1, Math.round((32 * box.value.h) / box.value.w)) : 32
  const px = decodeBlurhash(props.image.placeholder, w, h)
  const ctx = el.getContext('2d')
  if (!px || !ctx) return
  el.width = w
  el.height = h
  ctx.putImageData(new ImageData(px, w, h), 0, 0)
}

onMounted(() => {
  paint()
  // A cached image can finish before Vue attaches @load.
  if (pic.value?.complete && pic.value.naturalWidth > 0) loaded.value = true
})
watch(() => props.image.placeholder, paint)
watch(() => props.image.url, () => {
  loaded.value = false
  failed.value = false
})

const open = ref(false)
const closeBtn = ref<HTMLButtonElement | null>(null)
const opener = ref<HTMLElement | null>(null)

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') close()
}
async function show() {
  if (!hasUrl.value || uploading.value) return
  open.value = true
  window.addEventListener('keydown', onKey)
  await nextTick()
  closeBtn.value?.focus()
}
function close() {
  open.value = false
  window.removeEventListener('keydown', onKey)
  opener.value?.focus()
}
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))

// The lightbox box: the stored aspect ratio, as large as 92vw x 88vh allows.
const aspect = computed(() => props.image.width / props.image.height || 1)
const lightStyle = computed(() => ({
  aspectRatio: `${props.image.width} / ${props.image.height}`,
  width: `min(92vw, calc(88vh * ${aspect.value}))`,
}))
</script>

<template>
  <figure class="image" :style="figureStyle">
    <component
      :is="hasUrl && !uploading ? 'button' : 'div'"
      :ref="(el: any) => (opener = el)"
      class="frame"
      :class="{ openable: hasUrl && !uploading }"
      :style="frameStyle"
      :type="hasUrl && !uploading ? 'button' : undefined"
      :aria-label="hasUrl && !uploading ? `Open ${image.filename}` : undefined"
      @click="show"
    >
      <!-- The blurhash fills the box first; the plane stays flat without one. -->
      <canvas ref="blur" class="blur" aria-hidden="true" />
      <img
        v-if="hasUrl"
        ref="pic"
        class="pic"
        :class="{ shown: loaded }"
        :src="inline"
        :alt="image.filename"
        :width="image.width"
        :height="image.height"
        decoding="async"
        @load="loaded = true"
        @error="failed = true"
      />
      <div v-if="!loaded" class="badge">
        <span v-if="!failed" class="dot" />
        <span class="mono">{{
          failed ? 'UNAVAILABLE' : uploading ? `UPLOADING · ${percent}%` : hasUrl ? 'LOADING' : 'SENDING'
        }}</span>
      </div>
    </component>

    <figcaption class="strip">
      <span class="fname">{{ image.filename }}</span>
      <span class="tnum dims">{{ image.width }}×{{ image.height }} · {{ megabytes(image.bytes) }}</span>
    </figcaption>
  </figure>

  <Teleport to="body">
    <div v-if="open" class="lightbox" role="dialog" aria-modal="true" :aria-label="image.filename" @click.self="close">
      <div class="light-box" :style="lightStyle">
        <!-- The cached thumbnail underneath, the full image over it: the box
             is the stored ratio, so the full image arriving moves nothing. -->
        <img class="light-pic" :src="inline" alt="" :width="image.width" :height="image.height" />
        <img class="light-pic" :src="image.url" :alt="image.filename" :width="image.width" :height="image.height" />
      </div>
      <div class="light-bar">
        <span class="mono">{{ image.filename }} · {{ image.width }}×{{ image.height }} · {{ megabytes(image.bytes) }}</span>
        <button ref="closeBtn" class="light-close mono" type="button" @click="close">CLOSE · ESC</button>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.image {
  margin: 0;
  border: 1px solid var(--line-edge);
  background: var(--surface-lift);
}

.frame {
  position: relative;
  display: block;
  width: 100%;
  margin: 0;
  padding: 0;
  overflow: hidden;
  border: 0;
  background: var(--surface-raised);
}

.frame.openable {
  cursor: zoom-in;
}

.frame:focus-visible {
  outline: 2px solid var(--accent-wire);
  outline-offset: -2px;
}

/* Everything inside the frame is out of flow, so nothing painted here can
 * change the frame's size. */
.blur,
.pic {
  position: absolute;
  inset: 0;
  display: block;
  width: 100%;
  height: 100%;
}

.pic {
  object-fit: cover;
  opacity: 0;
}

.pic.shown {
  opacity: 1;
}

.badge {
  position: absolute;
  bottom: 12px;
  left: 12px;
  display: flex;
  gap: 8px;
  align-items: center;
}

.badge .mono {
  padding: 3px 6px;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.12em;
  color: var(--text-primary);
  background: color-mix(in srgb, var(--surface-base) 70%, transparent);
}

.dot {
  display: block;
  width: 5px;
  height: 5px;
  background: var(--accent-wire);
  animation: catPulse 1.2s ease-in-out infinite;
}

/* Unusual for a chat app; correct for a group that sends documentation
 * photos and needs to say "the 4471 one". */
.strip {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 6px 8px;
  font: var(--type-label);
  font-size: 9.5px;
  color: var(--text-meta);
  border-top: 1px solid var(--line-inner);
}

.fname {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dims {
  flex: none;
  white-space: nowrap;
}

/* The lightbox lives on body (Teleport). No motion, no overscroll. */
.lightbox {
  position: fixed;
  inset: 0;
  z-index: 50;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--s2);
  padding: 16px;
  overscroll-behavior: none;
  background: color-mix(in srgb, var(--surface-base) 92%, transparent);
}

.light-box {
  position: relative;
  max-height: 88vh;
  background: var(--surface-raised);
}

.light-pic {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  object-fit: contain;
}

.light-bar {
  display: flex;
  gap: 16px;
  align-items: center;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.12em;
  color: var(--text-meta);
}

.light-close {
  padding: 6px 10px;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.12em;
  color: var(--text-primary);
  cursor: pointer;
  background: transparent;
  border: 1px solid var(--line-edge);
}

.light-close:focus-visible {
  outline: 2px solid var(--accent-wire);
}
</style>
