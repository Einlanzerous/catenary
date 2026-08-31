<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import type { Message } from '@/types'
import { dayLabel, sameDay } from '@/lib/format'
import {
  activeConversation,
  activeMessages,
  newCount,
  state,
  typingLabel,
} from '@/store'
import Composer from './Composer.vue'
import ConnectionBanner from './ConnectionBanner.vue'
import MessageRow from './MessageRow.vue'

interface DateRule { kind: 'date'; key: string; label: string }
interface UnreadRule { kind: 'unread'; key: string; label: string }
interface Row { kind: 'message'; key: string; message: Message; previous?: Message }

const conversation = computed(() => activeConversation.value)

/**
 * The two rules that break the run of messages: a date change, and the point
 * the reader had got to. The unread rule is placed from `firstUnreadSeq`
 * rather than from a count, so it lands correctly after a resync.
 */
const rows = computed<(Row | DateRule | UnreadRule)[]>(() => {
  const out: (Row | DateRule | UnreadRule)[] = []
  const messages = activeMessages.value
  const firstUnread = conversation.value?.firstUnreadSeq
  let unreadDrawn = false

  messages.forEach((message, i) => {
    const previous = messages[i - 1]

    if (!previous || !sameDay(new Date(previous.at), new Date(message.at))) {
      out.push({ kind: 'date', key: `d-${message.id}`, label: dayLabel(message.at) })
    }

    if (firstUnread !== undefined && !unreadDrawn && message.seq >= firstUnread) {
      const count = newCount(conversation.value)
      if (count > 0) {
        out.push({ kind: 'unread', key: `u-${message.id}`, label: `${count} NEW` })
      }
      unreadDrawn = true
    }

    out.push({ kind: 'message', key: message.id, message, previous })
  })

  return out
})

const typing = computed(() => typingLabel(state.activeId))

const scroller = ref<HTMLElement | null>(null)

watch(
  () => [state.activeId, activeMessages.value.length],
  async () => {
    await nextTick()
    const el = scroller.value
    if (el) el.scrollTop = el.scrollHeight
  },
  { immediate: true },
)

// Arriving from a search hit or a reply stub scrolls the source into view.
watch(
  () => state.arrivedAt,
  async (id) => {
    if (!id) return
    await nextTick()
    scroller.value
      ?.querySelector(`[data-message="${id}"]`)
      ?.scrollIntoView({ block: 'center', behavior: 'smooth' })
  },
)
</script>

<template>
  <section class="thread">
    <header class="head">
      <h1 class="title">{{ conversation.name }}</h1>
      <!-- TLS, not E2E. D1 declines end-to-end encryption and names honesty
           about what the server can see as the mitigation, so the chip states
           the guarantee that actually holds: encrypted in transit. -->
      <span class="members">{{ conversation.memberCount }} MEMBERS · TLS</span>
      <nav class="tools">
        <button>SEARCH</button>
        <button>FILES</button>
        <button>MEMBERS</button>
        <button>⋯</button>
      </nav>
    </header>

    <ConnectionBanner />

    <div ref="scroller" class="stream scroll">
      <template v-for="row in rows" :key="row.key">
        <div v-if="row.kind === 'date'" class="rule">
          <span class="rule-label">{{ row.label }}</span>
          <span class="rule-line" />
        </div>

        <div v-else-if="row.kind === 'unread'" class="rule unread">
          <span class="rule-label">{{ row.label }}</span>
          <span class="rule-line" />
        </div>

        <MessageRow
          v-else
          :data-message="row.message.id"
          :message="row.message"
          :previous="row.previous"
          :member-count="conversation.memberCount"
        />
      </template>

      <!-- Deliberate call 10a: typing is the thread's own last row, in the
           message column — not a strip under the composer. The area you type
           in stays the bottom of the window, and the indicator appears
           exactly where the message will. -->
      <div v-if="typing" class="typing">
        <div class="typing-gutter" />
        <div class="typing-line">
          <span class="live" />
          <span class="names">{{ typing }}</span>
          <span class="dots">
            <i /><i /><i />
          </span>
        </div>
      </div>
    </div>

    <Composer :conversation-name="conversation.name" />
  </section>
</template>

<style scoped>
.thread {
  display: grid;
  grid-template-rows: 52px auto 1fr auto;
  min-width: 0;
  overflow: hidden;
  background: var(--surface-base);
}

.head {
  display: flex;
  gap: 14px;
  align-items: center;
  padding: 0 var(--s6);
  background: var(--surface-rail);
  border-bottom: 1px solid var(--line-hair);
}

.title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--text-primary);
}

.members {
  font: var(--type-label);
  font-size: 10.5px;
  letter-spacing: 0.08em;
  color: var(--text-meta);
}

.tools {
  display: flex;
  gap: 18px;
  margin-left: auto;
}

.tools button {
  font: var(--type-label);
  letter-spacing: var(--track-label);
  color: var(--text-meta);
}
.tools button:hover {
  color: var(--text-primary);
}

.stream {
  min-height: 0;
  padding: 10px 0 0;
}

.rule {
  display: grid;
  grid-template-columns: var(--gutter-w) 1fr;
  column-gap: var(--s4);
  align-items: center;
  margin: 4px 0 12px;
  padding-right: var(--s6);
}

.rule-label {
  font: var(--type-label);
  letter-spacing: 0.12em;
  color: var(--text-meta);
  text-align: right;
}

.rule-line {
  height: 1px;
  background: var(--line-hair);
}

.typing {
  display: grid;
  grid-template-columns: var(--gutter-w) 1fr;
  column-gap: var(--s4);
  padding: 10px var(--s6) 2px 0;
}

.typing-line {
  display: flex;
  gap: 9px;
  align-items: center;
}

.live {
  display: block;
  flex: none;
  width: 7px;
  height: 7px;
  background: var(--accent-wire);
  animation: catPulse 1.4s ease-in-out infinite;
}

.names {
  font-size: 13px;
  color: var(--text-secondary);
}

.dots {
  display: flex;
  gap: 3px;
  align-items: center;
}

.dots i {
  display: block;
  width: 3px;
  height: 3px;
  background: var(--text-meta);
  animation: catPulse 1.2s ease-in-out infinite;
}
.dots i:nth-child(2) {
  animation-delay: 0.2s;
}
.dots i:nth-child(3) {
  animation-delay: 0.4s;
}

.rule.unread {
  margin: 12px 0 10px;
}
.rule.unread .rule-label {
  color: var(--accent-wire);
}
.rule.unread .rule-line {
  background: var(--accent-rule);
}

@media (max-width: 900px) {
  .rule {
    grid-template-columns: auto 1fr;
    padding: 0 var(--s4);
  }
  .typing {
    grid-template-columns: 1fr;
    padding: 10px var(--s4) 2px;
  }
  .typing-gutter {
    display: none;
  }
  .head {
    padding: 0 var(--s4);
  }
  .tools {
    gap: 12px;
  }
}
</style>
