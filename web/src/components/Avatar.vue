<script setup lang="ts">
import { computed } from 'vue'
import { state, user } from '@/store'

const props = withDefaults(defineProps<{ userId: string; size?: number }>(), {
  size: 20,
})

const person = computed(() => user(props.userId))
const mine = computed(() => props.userId === state.me)
</script>

<template>
  <!-- A square, never a circle: radius is 0 everywhere except inputs. -->
  <span
    class="avatar"
    :class="{ mine }"
    :style="{
      width: `${size}px`,
      height: `${size}px`,
      fontSize: `${Math.round(size * 0.45)}px`,
    }"
    :title="person?.name"
    >{{ person?.initials }}</span
  >
</template>

<style scoped>
.avatar {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: center;
  font-family: var(--font-mono);
  background: var(--surface-avatar);
  color: var(--text-bright);
}

.avatar.mine {
  background: var(--accent-wire);
  color: var(--on-accent);
}
</style>
