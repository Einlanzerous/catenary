<script setup lang="ts">
import { computed, ref } from 'vue'
import { resample } from '@/lib/waveform'

/**
 * Renders stored peaks. It never generates them — deliberate call 13: the bar
 * pattern is identical in web and Flutter only because both draw the same
 * server-side array.
 */
const props = withDefaults(
  defineProps<{
    peaks: number[]
    /** 0–1. Played bars take the accent, the rest stay grey. */
    played?: number
    bars?: number
    height?: number
    dim?: boolean
    seekable?: boolean
  }>(),
  { played: 0, bars: 64, height: 32, dim: false, seekable: false },
)

const emit = defineEmits<{ seek: [fraction: number] }>()

const el = ref<HTMLElement | null>(null)

const shown = computed(() => resample(props.peaks, props.bars))

function onClick(event: MouseEvent) {
  if (!props.seekable || !el.value) return
  const box = el.value.getBoundingClientRect()
  emit('seek', Math.min(1, Math.max(0, (event.clientX - box.left) / box.width)))
}
</script>

<template>
  <div
    ref="el"
    class="wave"
    :class="{ dim, seekable }"
    :style="{ height: `${height}px` }"
    @click="onClick"
  >
    <i
      v-for="(peak, i) in shown"
      :key="i"
      :class="{ on: i / shown.length < played }"
      :style="{ height: `${peak}%` }"
    />
  </div>
</template>

<style scoped>
.wave {
  display: flex;
  align-items: center;
  gap: 2px;
  width: 100%;
  min-width: 0;
  overflow: hidden;
}

.wave.seekable {
  cursor: pointer;
}

.wave i {
  display: block;
  flex: none;
  width: 2px;
  background: var(--wave-rest);
}

.wave.dim i {
  background: var(--wave-rest-dim);
}

.wave i.on {
  background: var(--accent-wire);
}
</style>
