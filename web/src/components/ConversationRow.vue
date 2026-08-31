<script setup lang="ts">
import { computed } from 'vue'
import type { Conversation } from '@/types'
import { duration, railStamp, sameDay } from '@/lib/format'
import {
  isMine,
  lastMessageOf,
  select,
  state,
  unreadCount,
  user,
  voiceOf,
} from '@/store'

const props = defineProps<{ conversation: Conversation }>()

const active = computed(() => state.activeId === props.conversation.id)
const last = computed(() => lastMessageOf(props.conversation.id))
const unread = computed(() => unreadCount(props.conversation))

/** The rail's own row anatomy is identical for rooms and DMs. */
const preview = computed(() => {
  const m = last.value
  if (!m) return ''
  const voice = voiceOf(m)
  let body: string
  if (voice) {
    body =
      voice.transcript.state === 'pending'
        ? `transcript pending · ${duration(voice.durationSec)}`
        : `voice note · ${duration(voice.durationSec)}`
  } else if (m.attachments?.some((a) => a.kind === 'image')) {
    body = 'photo'
  } else {
    body = m.text ?? ''
  }

  if (isMine(m)) return `You: ${body}`
  // A DM's preview needs no name — the row is already the person.
  return props.conversation.kind === 'group'
    ? `${user(m.authorId)?.name.split(' ')[0]}: ${body}`
    : body
})

/** The trailing marker: unread count, own delivery state, or MUTED. */
const marker = computed(() => {
  if (unread.value > 0) {
    return { kind: 'count' as const, text: String(unread.value) }
  }
  if (props.conversation.muted) return { kind: 'label' as const, text: 'MUTED' }
  const m = last.value
  if (m && isMine(m)) {
    return {
      kind: m.state === 'failed' ? ('fault' as const) : ('label' as const),
      text: m.state.toUpperCase(),
    }
  }
  return null
})

/**
 * A conversation that has gone quiet dims its name; anything with unread stays
 * bright. Deliberate call 05 rejects bold-name-for-unread, and this is the
 * contrast that replaces it.
 *
 * NOTE — the call's own wording is "read rows dim their name", which would
 * also dim Sunday Dinner and Nadia. The canvas markup dims exactly the four
 * rows whose stamp is not a clock, so "quiet" is what it actually encodes,
 * and that is what this implements.
 */
const quiet = computed(() => {
  const m = last.value
  return !!m && unread.value === 0 && !sameDay(new Date(m.at), new Date())
})
</script>

<template>
  <button class="row" :class="{ active }" @click="select(conversation.id)">
    <!-- 3px accent spine, the active marker. -->
    <span class="spine" />
    <span class="cell">
      <span class="top">
        <span class="name" :class="{ quiet }">{{ conversation.name }}</span>
        <span
          class="stamp meta tnum"
          :class="{ unread: unread > 0 }"
          >{{ last ? railStamp(last.at) : '' }}</span
        >
      </span>
      <span class="bottom">
        <span class="preview" :class="{ unread: unread > 0 }">{{
          preview
        }}</span>
        <span
          v-if="marker"
          class="marker"
          :class="marker.kind"
          >{{ marker.text }}</span
        >
      </span>
    </span>
  </button>
</template>

<style scoped>
.row {
  display: grid;
  grid-template-columns: 3px 1fr;
  width: 100%;
  text-align: left;
}

.row:hover {
  background: var(--surface-lift);
}

.row.active {
  background: var(--surface-raised);
}

.spine {
  background: transparent;
}
.row.active .spine {
  background: var(--accent-wire);
}

.cell {
  display: block;
  min-width: 0;
  padding: 9px 16px 10px 17px;
}

.top,
.bottom {
  display: flex;
  gap: 10px;
  align-items: baseline;
  justify-content: space-between;
}

.bottom {
  margin-top: 3px;
}

.name {
  font: var(--type-sender);
  color: var(--text-primary);
}
.name.quiet {
  color: var(--text-secondary);
}

.stamp {
  flex: none;
  font-size: 10.5px;
}
.stamp.unread {
  color: var(--accent-wire);
}

.preview {
  overflow: hidden;
  font-size: 12.5px;
  color: var(--text-secondary);
  text-overflow: ellipsis;
  white-space: nowrap;
}
.preview.unread {
  color: var(--text-bright);
}

.marker {
  flex: none;
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
}

.marker.count {
  padding: 1px 5px;
  font-size: 10px;
  font-weight: 500;
  color: var(--on-accent);
  background: var(--accent-wire);
}

.marker.label {
  font-size: 9.5px;
  letter-spacing: 0.1em;
  color: var(--text-dim);
}

.marker.fault {
  font-size: 9.5px;
  letter-spacing: 0.1em;
  color: var(--signal-fault);
}
</style>
