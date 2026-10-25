<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import type { LinkAttachment } from '@/wire/generated'
import { decodeBlurhash } from '@/lib/blurhash'
import { imageBox, thumbUrl } from '@/lib/image-box'
import { hostOf, openableUrl, sameOrigin } from '@/linkpreview'
import { state } from '@/store'

/**
 * CANT-323. A link preview card: what the server's own fetch of `url` saw when
 * the sender composed the message, frozen into the message.
 *
 * WHAT IT WILL NOT CLAIM (CLAUDE.md invariant 3). The host line is derived
 * from `url` and never from the page's `site_name`. Title and description are
 * the page's own words and are drawn as text, below the host, never as markup
 * and never as a link target. No word here says verified, safe or secure, and
 * nothing is live: the card is a snapshot and no client contacts the target
 * until the reader taps it.
 *
 * THE IMAGE IS THIS SERVER'S. It is drawn only when it is on the session's own
 * origin, and as the `&v=thumb` variant like every inline image, so a stored
 * record can never make the page contact a third party. With stored dimensions
 * the box is final before the first byte (CANT-49's rule); without them there
 * is no image box at all, so there is nothing to reflow.
 */
const props = defineProps<{ link: LinkAttachment }>()

const host = computed(() => hostOf(props.link.url))
const href = computed(() => openableUrl(props.link.url))
const hasBox = computed(() => !!props.link.imageWidth && !!props.link.imageHeight)
const src = computed(() =>
  props.link.imageUrl && hasBox.value && sameOrigin(props.link.imageUrl, state.origin) ? thumbUrl(props.link.imageUrl) : '',
)
const box = computed(() => imageBox(props.link.imageWidth ?? 1, props.link.imageHeight ?? 1))
const figureStyle = computed(() => ({ width: `${box.value.w}px` }))
const frameStyle = computed(() => ({ height: `${box.value.h}px` }))

const loaded = ref(false)
const failed = ref(false)
const pic = ref<HTMLImageElement | null>(null)
const blur = ref<HTMLCanvasElement | null>(null)

function paint() {
  const el = blur.value
  if (!el || !props.link.imagePlaceholder || !hasBox.value) return
  const w = box.value.w >= box.value.h ? 32 : Math.max(1, Math.round((32 * box.value.w) / box.value.h))
  const h = box.value.w >= box.value.h ? Math.max(1, Math.round((32 * box.value.h) / box.value.w)) : 32
  const px = decodeBlurhash(props.link.imagePlaceholder, w, h)
  const ctx = el.getContext('2d')
  if (!px || !ctx) return
  el.width = w
  el.height = h
  ctx.putImageData(new ImageData(px, w, h), 0, 0)
}
onMounted(() => {
  paint()
  if (pic.value?.complete && pic.value.naturalWidth > 0) loaded.value = true
})
watch(() => props.link.imagePlaceholder, paint)
</script>

<template>
  <component
    :is="href ? 'a' : 'div'"
    class="card"
    :href="href ?? undefined"
    :target="href ? '_blank' : undefined"
    :rel="href ? 'noopener noreferrer' : undefined"
    :style="figureStyle"
  >
    <span v-if="src" class="frame" :style="frameStyle" aria-hidden="true">
      <canvas ref="blur" class="blur" />
      <img
        ref="pic"
        class="pic"
        :class="{ shown: loaded && !failed }"
        :src="src"
        alt=""
        :width="link.imageWidth"
        :height="link.imageHeight"
        decoding="async"
        @load="loaded = true"
        @error="failed = true"
      />
    </span>
    <span class="words">
      <span v-if="host" class="host">{{ host }}</span>
      <span v-if="link.title" class="title">{{ link.title }}</span>
      <span v-if="link.description" class="desc">{{ link.description }}</span>
    </span>
  </component>
</template>

<style scoped>
.card {
  display: block;
  max-width: 100%;
  color: inherit;
  text-decoration: none;
  background: var(--surface-lift);
  border: 1px solid var(--line-edge);
}

a.card:focus-visible {
  outline: 2px solid var(--accent-wire);
  outline-offset: 1px;
}

.frame {
  position: relative;
  display: block;
  overflow: hidden;
  background: var(--surface-raised);
}

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

.words {
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: 8px 10px 9px;
  border-top: 1px solid var(--line-inner);
}

.frame + .words {
  border-top: 1px solid var(--line-inner);
}

.card > .words:first-child {
  border-top: 0;
}

.host {
  overflow: hidden;
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.12em;
  color: var(--text-meta);
  text-overflow: ellipsis;
  text-transform: uppercase;
  white-space: nowrap;
}

.title {
  overflow-wrap: anywhere;
  font-size: 13.5px;
  font-weight: 600;
  line-height: 18px;
  color: var(--text-primary);
}

.desc {
  display: -webkit-box;
  overflow: hidden;
  overflow-wrap: anywhere;
  font-size: 12.5px;
  line-height: 17px;
  color: var(--text-secondary);
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 3;
}
</style>
