<script setup lang="ts">
import { computed } from 'vue'
import { duration } from '@/lib/format'
import { retryNow, state } from '@/store'

/**
 * Deliberate call 09: the reconnect banner counts. Attempt number and retry
 * countdown, not a spinner — you should be able to tell a stalled client from
 * a working one without opening devtools.
 */
const c = computed(() => state.connection)
const percent = computed(() =>
  c.value.total ? Math.round(((c.value.synced ?? 0) / c.value.total) * 100) : 0,
)
const n = (value: number | undefined) => (value ?? 0).toLocaleString('en-US')

/**
 * CANT-31 §6: the two terminal states, named, and what ends each. THE HONESTY
 * FLOOR, NOT THE DESIGN (CANT-35 criterion 30): nothing queued will drain from
 * here, so this branch says so and offers no retry — `retryNow()` cannot end a
 * terminal state, and a button that looked like it could would be the claim
 * Invariant 3 forbids. CANT-38 designs the real experience, re-enrollment
 * included.
 */
const terminal = computed(() => {
  const t = c.value.terminal
  if (c.value.state !== 'terminal' || !t) return null
  return t.kind === 'credential'
    ? { label: 'CREDENTIAL', text: 'This device is no longer signed in — re-enroll it to reconnect. Nothing sends until then.' }
    : { label: 'PROTOCOL', text: 'This app cannot talk to the server — update it, or report a bug. Nothing sends until then.' }
})
</script>

<template>
  <div v-if="c.state === 'reconnecting'" class="banner lost">
    <span class="dot pulse" />
    <span class="text">Connection lost — reconnecting</span>
    <span class="count tnum">
      attempt {{ c.attempt }} · retry in {{ duration(c.retryInSec) }}
    </span>
    <button class="action" @click="retryNow">RETRY NOW</button>
  </div>

  <div v-else-if="c.state === 'offline'" class="banner lost">
    <span class="dot" />
    <span class="text">Offline — messages will queue</span>
    <button class="action" @click="retryNow">RECONNECT</button>
  </div>

  <div v-else-if="terminal" class="banner lost terminal">
    <span class="dot" />
    <span class="text">{{ terminal.text }}</span>
    <span class="count">{{ terminal.label }}</span>
  </div>

  <div v-else-if="c.state === 'resyncing'" class="banner catchup">
    <div class="line">
      <span class="dot" />
      <span class="text">Reconnected — catching up</span>
      <!-- Progress in this product is always numeric. -->
      <!-- Only when there is a number to show: CANT-37 owns where the
           transport's progress comes from, and a 0 / 0 would be a claim. -->
      <span v-if="c.total !== undefined" class="count tnum">
        {{ n(c.synced) }} / {{ n(c.total) }} messages
      </span>
      <span v-if="c.roomsPending !== undefined" class="pending">{{ c.roomsPending }} ROOMS PENDING</span>
    </div>
    <div class="track"><i :style="{ width: `${percent}%` }" /></div>
  </div>
</template>

<style scoped>
.banner {
  padding: 9px var(--s6);
  border-bottom: 1px solid var(--line-hair);
}

.banner.lost {
  display: flex;
  gap: 10px;
  align-items: center;
  background: var(--accent-wash);
  border-bottom-color: var(--accent-rule);
}

.banner.terminal .count {
  margin-left: auto;
  letter-spacing: var(--track-label);
}

.banner.catchup {
  padding: 10px var(--s6) 11px;
  background: var(--accent-wash-soft);
}

.line {
  display: flex;
  gap: 12px;
  align-items: center;
}

.dot {
  display: block;
  flex: none;
  width: 6px;
  height: 6px;
  background: var(--accent-wire);
}
.pulse {
  animation: catPulse 1.2s ease-in-out infinite;
}

.text {
  font-size: 12.5px;
  color: var(--text-primary);
}

.count {
  font: var(--type-meta);
  font-size: 10.5px;
  color: var(--accent-dim);
}
.catchup .count {
  color: var(--text-secondary);
}

.pending {
  margin-left: auto;
  font: var(--type-label);
  letter-spacing: 0.1em;
  color: var(--text-meta);
}

.action {
  margin-left: auto;
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--accent-wire);
}

.track {
  height: 2px;
  margin-top: 10px;
  overflow: hidden;
  background: var(--line-edge);
}
.track i {
  display: block;
  height: 100%;
  background: var(--accent-wire);
  transition: width 0.12s linear;
}
</style>
