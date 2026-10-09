<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { isOutboxMessage, type RenderedMessage } from '@/client-types'
import { dayLabel, sameDay } from '@/lib/format'
import {
  activeConversation,
  activeLog,
  activeTail,
  conversationTitle,
  isSelf,
  newCount,
  otherMember,
  persistNotice,
  state,
  transportLabel,
  typingLabel,
} from '@/store'
import Composer from './Composer.vue'
import ConnectionBanner from './ConnectionBanner.vue'
import MessageRow from './MessageRow.vue'

interface DateRule { kind: 'date'; key: string; label: string }
interface UnreadRule { kind: 'unread'; key: string; label: string }
interface Row { kind: 'message'; key: string; message: RenderedMessage; previous?: RenderedMessage }

/** App renders this pane only while there is an active conversation. */
const conversation = computed(() => activeConversation.value!)

/** The header chip: the kind's word, then the transport. See the template. */
const chip = computed(() => {
  const c = conversation.value
  // JUST YOU states the one membership fact the server keeps for a `self`
  // conversation (exactly one member), and its last word is derived from the
  // origin like every other chip's, never a literal (CANT-254, Invariant 3).
  if (isSelf(c)) return `JUST YOU · ${transportLabel.value}`
  return c.kind === 'direct'
    ? `DIRECT · ${transportLabel.value}`
    : `${c.memberCount} MEMBERS · ${transportLabel.value}`
})

/** The header's title and what Composer's placeholder names (CANT-141): the
 *  other member's live name once it is known, `conversation.name` only as
 *  the fallback until it is. See conversationTitle's own doc for why one
 *  helper rather than reading `conversation.name` here directly. */
const title = computed(() => conversationTitle(conversation.value))

/** CANT-138: the other half of a direct conversation cannot read what is
 *  being typed at them — the header says so, quietly, when it is true. */
const otherDeactivated = computed(() => {
  const c = conversation.value
  return c?.kind === 'direct' && !!otherMember(c)?.deactivated
})

/**
 * The two rules that break the run of messages: a date change, and the point
 * the reader had got to. The unread rule is placed from `firstUnreadSeq`
 * rather than from a count, so it lands correctly after a resync.
 *
 * After the log comes the outbox's tail (CANT-36): unacked entries, in the
 * order they were composed, never sorted in among the seqs — they have none.
 * An acked entry is already in the log at the server's seq. Neither kind can
 * place the unread rule; only a server record can.
 */
const rows = computed<(Row | DateRule | UnreadRule)[]>(() => {
  const out: (Row | DateRule | UnreadRule)[] = []
  const messages = [...activeLog.value, ...activeTail.value]
  const firstUnread = conversation.value?.firstUnreadSeq
  let unreadDrawn = false

  messages.forEach((message, i) => {
    const previous = messages[i - 1]

    if (!previous || !sameDay(new Date(previous.at), new Date(message.at))) {
      out.push({ kind: 'date', key: `d-${message.id}`, label: dayLabel(message.at) })
    }

    if (
      firstUnread !== undefined &&
      !unreadDrawn &&
      !isOutboxMessage(message) &&
      message.seq >= firstUnread
    ) {
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
  () => [state.activeId, activeLog.value.length, activeTail.value.length],
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
      <h1 class="title">{{ title }}</h1>
      <!-- Quiet and factual, never the copper accent: this reads "cannot
           read this any more", not an alarm. -->
      <span v-if="otherDeactivated" class="deactivated-tag">DEACTIVATED</span>
      <!-- TLS, not E2E. D1 declines end-to-end encryption and names honesty
           about what the server can see as the mitigation, so the chip states
           the guarantee that actually holds: encrypted in transit. And TLS
           only where it is (CANT-221): the word is derived from the origin
           the session talks to, and over `http://` it reads CLEARTEXT, the
           same word the Flutter client shows for the same server.
           The first half branches on kind (CANT-248): a direct reads DIRECT,
           the rail's own word and the app's, because its count is always two
           and goes false the moment the other member is deactivated. Any
           other kind, the wire's `unknown` sentinel included, states the
           count: that is a fact the server sent, where DIRECT would claim a
           kind this build did not recognize. A `self` conversation reads
           JUST YOU (CANT-254): one member, which the server keeps; never
           E2E, and its last word is derived from the origin. -->
      <span class="members">{{ chip }}</span>
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

      <!-- CANT-36 §9: the browser has not granted persistence, and something
           here is unsent. A standing line in the tail, never a toast. -->
      <p v-if="persistNotice" class="persist-notice">{{ persistNotice }}</p>

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

    <Composer :conversation-name="title" />
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

/* CANT-138: dimmed, not alarmed — the copper accent is spoken for. */
.deactivated-tag {
  font: var(--type-label);
  font-size: 9.5px;
  letter-spacing: 0.1em;
  color: var(--text-dim);
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

.persist-notice {
  margin: 6px 0 0;
  padding: 0 var(--s6) 0 calc(var(--gutter-w) + var(--s4));
  font: var(--type-meta);
  font-size: 10.5px;
  color: var(--text-meta);
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
  .persist-notice {
    padding: 0 var(--s4);
  }
  .head {
    padding: 0 var(--s4);
  }
  .tools {
    gap: 12px;
  }
}
</style>
