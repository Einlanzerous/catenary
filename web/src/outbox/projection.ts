/* The render projection: an outbox item as the row components take it.
 *
 * An acked entry is placed by the SERVER's ack — its conversationId, seq and
 * at — never by its own conversationId or composedAt (`socket.go` builds the
 * ack from what was stored, and dedup is per author, not per conversation).
 * An unacked one carries no seq at all and renders in the tail by `order`.
 */

import type { Attachment, OutboxMessage } from '@/client-types'
import { BARE_1008_MESSAGE } from './outbox'
import type { OutboxError, OutboxItem } from './types'

export function errorText(e: OutboxError | undefined): string | undefined {
  if (!e) return undefined
  switch (e.kind) {
    case 'server':
    case 'upload':
      return e.message
    case 'bare_1008':
      return BARE_1008_MESSAGE
  }
}

export function project(item: OutboxItem): OutboxMessage {
  const { entry, ack } = item
  const attachments: Attachment[] = (entry.attachments ?? []).map((a) =>
    a.kind === 'voice'
      ? {
          kind: 'voice',
          // No media URL until an upload exists, and no peaks: those are
          // computed server-side, once, and never synthesized by a client.
          url: '',
          durationMs: a.durationMs ?? 0,
          peaks: [],
          transcript: { state: 'pending' },
        }
      : {
          kind: 'image',
          url: '',
          filename: a.filename ?? 'image',
          width: 1,
          height: 1,
          bytes: a.blob.size,
        },
  )
  return {
    id: entry.clientId,
    clientId: entry.clientId,
    pending: true,
    conversationId: ack?.conversationId ?? entry.conversationId,
    ...(ack ? { seq: ack.seq } : {}),
    authorId: entry.accountId,
    at: ack?.at ?? entry.composedAt,
    ...(entry.text !== undefined ? { text: entry.text } : {}),
    ...(attachments.length ? { attachments } : {}),
    ...(entry.replyPreview ? { replyTo: entry.replyPreview } : {}),
    state: item.state,
    ...(item.state === 'failed' ? { error: errorText(entry.lastError) } : {}),
    retrying: item.retrying,
  }
}
