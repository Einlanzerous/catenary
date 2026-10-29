<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import type { LinkAttachment } from '@/wire/generated'
import { decodeBlurhash } from '@/lib/blurhash'
import { linkImageAspect, thumbUrl } from '@/lib/image-box'
import { finalHostOf, hostOf, openableUrl, plainText, sameOrigin } from '@/linkpreview'
import { state } from '@/store'

/**
 * CANT-323. A link preview card: what the server's own fetch of `url` saw when
 * the sender composed the message, frozen into the message.
 *
 * WHAT IT WILL NOT CLAIM (CLAUDE.md invariant 3). The host line is derived
 * from `url` and never from the page's `site_name`. A redirect to another site
 * adds `→ final_host` after it, the server's word and not ours (CANT-338). Title and description are
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
const props = defineProps<{ link: LinkAttachment; own?: boolean }>()

const host = computed(() => hostOf(props.link.url))
const finalHost = computed(() => finalHostOf(props.link.finalHost))
const href = computed(() => openableUrl(props.link.url))
const title = computed(() => plainText(props.link.title))
const desc = computed(() => plainText(props.link.description))
/** An image is drawn only with BOTH stored dimensions and a stored blurhash
 *  (canvas 08, "image box"): either missing and the card is text-only. A ratio
 *  is never guessed and nothing reflows when the bytes land. */
const hasBox = computed(
  () => !!props.link.imageWidth && !!props.link.imageHeight && !!props.link.imagePlaceholder,
)
const src = computed(() =>
  props.link.imageUrl && hasBox.value && sameOrigin(props.link.imageUrl, state.origin) ? thumbUrl(props.link.imageUrl) : '',
)
const aspect = computed(() => linkImageAspect(props.link.imageWidth ?? 1, props.link.imageHeight ?? 1))
const frameStyle = computed(() => ({ aspectRatio: `${aspect.value.w} / ${aspect.value.h}` }))

const loaded = ref(false)
const failed = ref(false)
const pic = ref<HTMLImageElement | null>(null)
const blur = ref<HTMLCanvasElement | null>(null)

/** Decoded at 32x32 and scaled up by CSS, whatever the box (canvas 08). */
function paint() {
  const el = blur.value
  if (!el || !props.link.imagePlaceholder || !hasBox.value) return
  const px = decodeBlurhash(props.link.imagePlaceholder, 32, 32)
  const ctx = el.getContext('2d')
  if (!px || !ctx) return
  el.width = 32
  el.height = 32
  ctx.putImageData(new ImageData(px, 32, 32), 0, 0)
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
    :class="{ own }"
    :href="href ?? undefined"
    :target="href ? '_blank' : undefined"
    :rel="href ? 'noopener noreferrer' : undefined"
    :title="link.url"
    data-card
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
      <span v-if="host" class="host"
        ><span class="host-name"
          >{{ host }}<template v-if="finalHost"
            >{{ ' ' }}<span class="arrow" aria-label="redirects to">→</span>{{ ' ' + finalHost }}</template
          ></span
        ><span class="out" aria-hidden="true">↗</span></span
      >
      <span v-if="title" class="title">{{ title }}</span>
      <span v-if="desc" class="desc">{{ desc }}</span>
    </span>
  </component>
</template>

<style scoped>
/* The whole card is one target. Colour is the only motion (80 ms linear), no
 * transform, no accent: nothing here vouches for the page. Ground is one step
 * off the row it sits on: surface.rail on others' rows, surface.base on yours. */
.card {
  display: block;
  width: 100%;
  max-width: 440px;
  color: inherit;
  text-decoration: none;
  background: var(--surface-rail);
  border: 1px solid var(--line-hair);
  transition:
    background-color 80ms linear,
    border-color 80ms linear;
}
.card.own {
  background: var(--surface-base);
}

@media (hover: hover) {
  a.card:hover {
    background: var(--surface-raised);
  }
  a.card:hover .out {
    color: var(--text-primary);
  }
}

a.card:focus-visible {
  /* TODO(CANT-332): a focus-ring token; the canvas's interim is text.primary. */
  outline: 2px solid var(--text-primary);
  outline-offset: 2px;
}

a.card:active {
  /* TODO(CANT-332): the pressed border borrows text.meta; line.strong is the candidate. */
  background: var(--surface-raised);
  border-color: var(--text-meta);
}
a.card:active .out {
  color: var(--text-primary);
}

.frame {
  position: relative;
  display: block;
  overflow: hidden;
  background: var(--surface-raised);
  border-bottom: 1px solid var(--line-hair);
}

.blur,
.pic {
  position: absolute;
  inset: 0;
  display: block;
  width: 100%;
  height: 100%;
}

/* The image crossfades over the blurhash, linear opacity, 160 ms. If it never
 * loads the blurhash stays; nothing collapses. */
.pic {
  object-fit: cover;
  opacity: 0;
  transition: opacity 160ms linear;
}

.pic.shown {
  opacity: 1;
}

.words {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 4px;
  padding: 10px 12px 12px;
}

.host {
  display: flex;
  gap: 12px;
  justify-content: space-between;
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 14px;
  color: var(--text-secondary);
}
.host-name {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.arrow {
  color: var(--text-meta);
}
.out {
  flex: none;
  transition: color 80ms linear;
}

.title,
.desc {
  display: -webkit-box;
  overflow: hidden;
  overflow-wrap: anywhere;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
}
.title {
  font-size: 14px;
  font-weight: 600;
  line-height: 20px;
  color: var(--text-primary);
}
.desc {
  font-size: 13px;
  line-height: 20px;
  color: var(--text-secondary);
}

@media (max-width: 900px) {
  .frame {
    max-height: 240px;
  }
  .words {
    gap: 3px;
  }
  .host {
    font-size: 10.5px;
  }
  .desc {
    line-height: 19px;
  }
}
</style>
