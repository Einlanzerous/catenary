<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import type { ImageAttachment } from '@/types'
import { megabytes } from '@/lib/format'

const props = defineProps<{ image: ImageAttachment }>()

const MAX_W = 340
const MAX_H = 400

/**
 * The box is computed from the *stored* aspect ratio, so the row height is
 * final before a byte of the image arrives. No reflow, no jumping scroll
 * position mid-read.
 */
const box = computed(() => {
  const aspect = props.image.width / props.image.height
  let w = MAX_W
  let h = Math.round(w / aspect)
  if (h > MAX_H) {
    h = MAX_H
    w = Math.round(h * aspect)
  }
  return { width: `${w}px`, height: `${h}px` }
})

const uploading = computed(() => props.image.uploadedBytes !== undefined)

// Stands in for the real fetch: blurhash first, bytes second.
const loaded = ref(false)
const percent = ref(0)
let timer: ReturnType<typeof setInterval> | null = null

onMounted(() => {
  if (uploading.value) return
  timer = setInterval(() => {
    percent.value = Math.min(100, percent.value + 17)
    if (percent.value >= 100) {
      if (timer) clearInterval(timer)
      timer = null
      loaded.value = true
    }
  }, 110)
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <figure class="image" :style="{ width: box.width }">
    <div class="frame" :style="box">
      <!-- Blurhash stands in until the bytes land. -->
      <div v-if="!loaded" class="blur">
        <div class="badge">
          <span class="dot" />
          <span class="mono">LOADING · {{ percent }}%</span>
        </div>
      </div>
      <!-- Call 17: striped fills are placeholders for real photography, not a
           design element. Drop real imagery in before review. -->
      <div v-else class="stripe">
        <span class="mono caption">PHOTO — {{ image.filename }}</span>
      </div>
    </div>

    <figcaption class="strip">
      <span>{{ image.filename }}</span>
      <span class="tnum">
        {{ image.width }}×{{ image.height }}
        <template v-if="loaded"> · {{ megabytes(image.bytes) }}</template>
      </span>
    </figcaption>
  </figure>
</template>

<style scoped>
.image {
  margin: 0;
  border: 1px solid var(--line-edge);
  background: var(--surface-lift);
}

.frame {
  position: relative;
  overflow: hidden;
}

.blur {
  width: 100%;
  height: 100%;
  filter: saturate(0.8);
  background:
    radial-gradient(60% 70% at 22% 28%, #4a4034 0%, rgba(74, 64, 52, 0) 60%),
    radial-gradient(55% 60% at 78% 35%, #2d3a3e 0%, rgba(45, 58, 62, 0) 62%),
    radial-gradient(70% 80% at 50% 92%, #1c2426 0%, rgba(28, 36, 38, 0) 70%),
    #262b2a;
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

.stripe {
  display: flex;
  width: 100%;
  height: 100%;
  align-items: center;
  justify-content: center;
  background-image: repeating-linear-gradient(
    135deg,
    color-mix(in srgb, var(--surface-raised) 85%, var(--surface-base)) 0 8px,
    var(--surface-lift) 8px 16px
  );
}

.caption {
  font: var(--type-label);
  letter-spacing: 0.12em;
  color: var(--text-placeholder);
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
</style>
