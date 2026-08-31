<script setup lang="ts">
import { computed } from 'vue'
import type { Message } from '@/types'

/**
 * Deliberate call 03: status as words, not tick glyphs. Rendered in a fixed
 * 96px column so nothing reflows as state advances.
 *
 * NOTE — one reconciliation. The canvas's status ladder and its call 03 text
 * put READ in the accent ("the only state that spends the accent"), while all
 * four status labels actually rendered in its thread screens are meta grey,
 * as is the rail's READ. Four renderings against one legend: this follows the
 * renderings. Flip `.read` below if the legend was the intent.
 */
const props = defineProps<{ message: Message; memberCount: number }>()

const text = computed(() => {
  const m = props.message
  switch (m.state) {
    case 'sending':
      return 'SENDING'
    case 'queued':
      return 'QUEUED'
    case 'sent':
      return 'SENT'
    case 'delivered':
      return 'DELIVERED'
    case 'failed':
      return 'FAILED'
    case 'read':
      // In rooms the fraction is the useful part; hover lists who.
      return props.memberCount > 2 && m.readBy
        ? `READ ${m.readBy}/${props.memberCount}`
        : 'READ'
  }
})
</script>

<template>
  <div class="status" :class="message.state">
    <span>{{ text }}</span>
    <!-- Hairline sweep, no spinner. -->
    <div v-if="message.state === 'sending'" class="sweep"><i /></div>
  </div>
</template>

<style scoped>
.status {
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.12em;
  color: var(--text-meta);
  text-align: right;
}

.status.failed {
  color: var(--signal-fault);
}

.status.read {
  color: var(--text-meta);
}

.sweep {
  position: relative;
  height: 2px;
  margin-top: 6px;
  overflow: hidden;
  background: var(--line-edge);
}

.sweep i {
  position: absolute;
  inset: 0;
  width: 30%;
  background: var(--accent-wire);
  animation: catSweep 1.1s linear infinite;
}
</style>
