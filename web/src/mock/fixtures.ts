/* The design canvas's corpus, as data.
 *
 * Everything the canvas draws by hand lives here instead, so the components
 * render from a shape the server will eventually supply. Where the canvas is
 * internally inconsistent — its rail says Kitchen Table last moved at 14:12
 * while its replies section runs to 14:41 — the app derives rather than
 * hardcodes, so the rail shows 14:41. Deriving is the point; the canvas is a
 * mock of one moment.
 */

import type { Conversation, Message, User } from '@/types'
import { peaksFromSeed } from '@/lib/waveform'

export const ME = 'u-hollis'

export const USERS: Record<string, User> = {
  'u-hollis': { id: 'u-hollis', name: 'Hollis Byrne', initials: 'HB' },
  'u-ilse': { id: 'u-ilse', name: 'Ilse Marchetti', initials: 'IM' },
  'u-nadia': { id: 'u-nadia', name: 'Nadia Okonkwo', initials: 'NO' },
  'u-marek': { id: 'u-marek', name: 'Marek Dubois', initials: 'MD' },
  'u-ted': { id: 'u-ted', name: 'Ted Almasy', initials: 'TA' },
  'u-rosa': { id: 'u-rosa', name: 'Rosa Whitfield', initials: 'RW' },
}

export const CONVERSATIONS: Conversation[] = [
  { id: 'c-kitchen', kind: 'group', name: 'Kitchen Table', memberCount: 7, firstUnreadSeq: 1183, headSeq: 1192 },
  { id: 'c-bergen', kind: 'group', name: 'Bergen Hill Co-op', memberCount: 14, firstUnreadSeq: 412, headSeq: 413 },
  { id: 'c-sunday', kind: 'group', name: 'Sunday Dinner', memberCount: 9, headSeq: 88 },
  { id: 'c-shed', kind: 'group', name: 'Shed Projects', memberCount: 5, muted: true, headSeq: 240 },
  { id: 'c-ilse', kind: 'direct', name: 'Ilse Marchetti', memberCount: 2, firstUnreadSeq: 77, headSeq: 77 },
  { id: 'c-marek', kind: 'direct', name: 'Marek Dubois', memberCount: 2, headSeq: 51 },
  { id: 'c-nadia', kind: 'direct', name: 'Nadia Okonkwo', memberCount: 2, headSeq: 133 },
  { id: 'c-ted', kind: 'direct', name: 'Ted Almasy', memberCount: 2, headSeq: 19 },
  { id: 'c-rosa', kind: 'direct', name: 'Rosa Whitfield', memberCount: 2, headSeq: 64 },
]

/**
 * Timestamps are relative to the day this is opened, not to the canvas's
 * 15 AUG. The rail's whole vocabulary is relative — a clock for today, a
 * weekday for this week, a date for older — so pinning the fixtures to a
 * fixed date makes every row read as stale within a week and dims the entire
 * list. The shape is what is being demonstrated, not the date.
 */
function at(daysAgo: number, hm: string): string {
  const d = new Date()
  d.setDate(d.getDate() - daysAgo)
  const [h, m] = hm.split(':').map(Number)
  d.setHours(h, m, 0, 0)
  return d.toISOString()
}

const ILSE_TRANSCRIPT =
  'Okay so I talked to Ted about the delivery and the short version is Thursday ' +
  'still works but they want everything staged by two, which means somebody has ' +
  'to be at the shed in the morning to sign for the pallet. If nobody can do ' +
  'that I’ll ask them to hold it until Friday, but then we’re into the ' +
  'weekend and I’d rather not. Also the tension gauge came back from the ' +
  'shop, it’s in the blue case on the bench.'

/** Word offsets, so search can JUMP TO the matched word rather than the message. */
const ILSE_SEGMENTS = [
  { at: 0, text: 'Okay so I talked to Ted about the delivery and' },
  { at: 6, text: 'the short version is Thursday still works but they want' },
  { at: 13, text: 'everything staged by two, which means somebody has to be' },
  { at: 20, text: 'at the shed in the morning to sign for the pallet.' },
  { at: 27, text: 'If nobody can do that I’ll ask them to hold it until Friday,' },
  { at: 33, text: 'but then we’re into the weekend and I’d rather not.' },
  { at: 38, text: 'Also the tension gauge came back from the shop, it’s in the blue case on the bench.' },
]

export const MESSAGES: Message[] = [
  // ── Kitchen Table ──────────────────────────────────────────────────────
  {
    id: 'm-1181', seq: 1181, conversationId: 'c-kitchen', authorId: 'u-nadia',
    at: at(0, '13:41'), state: 'read',
    text: 'Anyone know whether the co-op still does the Thursday pickup, or did that move for the summer? I’ve got a standing order I’d rather not lose.',
  },
  {
    id: 'm-1182', seq: 1182, conversationId: 'c-kitchen', authorId: ME,
    at: at(0, '13:52'), state: 'read', readBy: 7,
    text: 'Still Thursday. They moved the window, not the day — 2 to 6 now instead of noon.',
  },
  // ── 3 NEW falls here (firstUnreadSeq 1183) ─────────────────────────────
  {
    id: 'm-1183', seq: 1183, conversationId: 'c-kitchen', authorId: 'u-ilse',
    at: at(0, '14:12'), state: 'delivered',
    attachments: [
      {
        kind: 'voice',
        durationSec: 38,
        peaks: peaksFromSeed(9931, 96),
        transcript: {
          state: 'ready',
          text: ILSE_TRANSCRIPT,
          segments: ILSE_SEGMENTS,
          engine: 'whisper-l3',
          language: 'en',
        },
      },
    ],
  },
  {
    id: 'm-1184', seq: 1184, conversationId: 'c-kitchen', authorId: 'u-ilse',
    at: at(0, '14:13'), state: 'delivered',
    text: 'photo from this morning, the whole run is re-tensioned',
  },
  {
    id: 'm-1185', seq: 1185, conversationId: 'c-kitchen', authorId: 'u-ilse',
    at: at(0, '14:13'), state: 'delivered',
    attachments: [
      { kind: 'image', filename: 'IMG_4471.HEIC', width: 3024, height: 2016, bytes: 2_202_009 },
    ],
  },
  {
    id: 'm-1186', seq: 1186, conversationId: 'c-kitchen', authorId: ME,
    at: at(0, '14:15'), state: 'delivered',
    text: 'I can be there at eight to sign for it.',
  },
  {
    id: 'm-1187', seq: 1187, conversationId: 'c-kitchen', authorId: ME,
    at: at(0, '14:16'), state: 'sent',
    text: 'bringing coffee for whoever else shows up',
  },
  {
    id: 'm-1188', seq: 1188, conversationId: 'c-kitchen', authorId: 'u-nadia',
    at: at(0, '14:20'), state: 'delivered',
    text: 'depot hours are here if anyone needs them — riverline.org/depot-hours',
  },
  {
    id: 'm-1189', seq: 1189, conversationId: 'c-kitchen', authorId: 'u-marek',
    at: at(0, '14:28'), state: 'delivered',
    attachments: [
      {
        kind: 'voice',
        durationSec: 52,
        peaks: peaksFromSeed(6151, 96),
        // Pending: playable immediately, not searchable yet, stub says so.
        transcript: { state: 'pending', etaSec: 20 },
      },
    ],
  },
  {
    id: 'm-1190', seq: 1190, conversationId: 'c-kitchen', authorId: 'u-nadia',
    at: at(0, '14:31'), state: 'delivered',
    text: 'Perfect, that’s the one I needed. Standing order is safe then.',
    replyTo: { messageId: 'm-1182', authorId: ME, kind: 'text', preview: 'Still Thursday. They moved the window, not the day — 2 to 6 now instead of noon.' },
  },
  {
    id: 'm-1191', seq: 1191, conversationId: 'c-kitchen', authorId: ME,
    at: at(0, '14:33'), state: 'read', readBy: 7,
    text: 'Staging by two is fine. I’ll be at the shed from eight.',
    replyTo: { messageId: 'm-1183', authorId: 'u-ilse', kind: 'voice', durationSec: 38, preview: ILSE_TRANSCRIPT },
  },
  {
    id: 'm-1192', seq: 1192, conversationId: 'c-kitchen', authorId: 'u-marek',
    at: at(0, '14:35'), state: 'delivered',
    text: 'That span looks straighter than it did in March.',
    replyTo: { messageId: 'm-1185', authorId: 'u-ilse', kind: 'image', preview: 'IMG_4471.HEIC' },
  },
  {
    id: 'm-1193', seq: 1193, conversationId: 'c-kitchen', authorId: 'u-ted',
    at: at(0, '14:38'), state: 'delivered',
    text: 'This is out of date — they haven’t updated it since the window moved.',
    replyTo: { messageId: 'm-1188', authorId: 'u-nadia', kind: 'link', preview: 'riverline.org/depot-hours', url: 'https://riverline.org/depot-hours' },
  },
  {
    id: 'm-1194', seq: 1194, conversationId: 'c-kitchen', authorId: 'u-rosa',
    at: at(0, '14:41'), state: 'delivered',
    text: 'Listened to it, I’ll take Friday morning.',
    // Source transcript is still pending — the stub says so rather than
    // showing an empty quote, and back-fills in place when it lands.
    replyTo: { messageId: 'm-1189', authorId: 'u-marek', kind: 'voice', durationSec: 52, preview: '' },
  },

  // ── Bergen Hill Co-op ──────────────────────────────────────────────────
  {
    id: 'm-b412', seq: 412, conversationId: 'c-bergen', authorId: 'u-ted',
    at: at(0, '13:55'), state: 'delivered',
    text: 'pallet is confirmed for the 2 to 6 window, depot said they’ll call ahead',
  },
  {
    id: 'm-b413', seq: 413, conversationId: 'c-bergen', authorId: 'u-ted',
    at: at(0, '13:58'), state: 'delivered',
    text: 'the delivery window moved to Thursday afternoon, I’ll confirm with the depot in the morning',
  },

  // ── Sunday Dinner ──────────────────────────────────────────────────────
  {
    id: 'm-s087', seq: 87, conversationId: 'c-sunday', authorId: 'u-rosa',
    at: at(4, '20:14'), state: 'delivered',
    text: 'every Thursday since March, same table, same time',
  },
  {
    id: 'm-s088', seq: 88, conversationId: 'c-sunday', authorId: ME,
    at: at(0, '11:20'), state: 'read', readBy: 9,
    text: 'bringing the big pot',
  },

  // ── Shed Projects (muted) ──────────────────────────────────────────────
  {
    id: 'm-h240', seq: 240, conversationId: 'c-shed', authorId: 'u-marek',
    at: at(5, '16:02'), state: 'delivered',
    attachments: [
      { kind: 'image', filename: 'shed_wall.jpg', width: 2016, height: 3024, bytes: 3_355_443 },
    ],
  },

  // ── DM: Ilse — unread voice note, transcript still pending ─────────────
  {
    id: 'm-i077', seq: 77, conversationId: 'c-ilse', authorId: 'u-ilse',
    at: at(0, '14:04'), state: 'delivered',
    attachments: [
      {
        kind: 'voice',
        durationSec: 72,
        peaks: peaksFromSeed(4477, 96),
        transcript: { state: 'pending', etaSec: 20 },
      },
    ],
  },

  // ── DM: Marek ──────────────────────────────────────────────────────────
  {
    id: 'm-k051', seq: 51, conversationId: 'c-marek', authorId: ME,
    at: at(0, '12:47'), state: 'sent',
    text: 'sent it to your inbox instead',
  },

  // ── DM: Nadia ──────────────────────────────────────────────────────────
  {
    id: 'm-n132', seq: 132, conversationId: 'c-nadia', authorId: 'u-nadia',
    at: at(1, '18:03'), state: 'delivered',
    attachments: [
      {
        kind: 'voice',
        durationSec: 107,
        peaks: peaksFromSeed(1777, 96),
        transcript: {
          state: 'ready',
          text: 'I can juggle the pickup either way, just tell me which day. If it’s Thursday I can take the truck, otherwise somebody else has to, because I’ve got the closing shift and no rush on any of it, truly.',
          segments: [
            { at: 0, text: 'I can juggle the pickup either way, just tell me which day.' },
            { at: 66, text: 'If it’s Thursday I can take the truck, otherwise somebody else has to,' },
            { at: 88, text: 'because I’ve got the closing shift and no rush on any of it, truly.' },
          ],
          engine: 'whisper-l3',
          language: 'en',
        },
      },
    ],
  },
  {
    id: 'm-n133', seq: 133, conversationId: 'c-nadia', authorId: 'u-nadia',
    at: at(0, '09:41'), state: 'delivered',
    text: 'no rush on any of it, truly',
  },

  // ── DM: Ted — the failed send the rail reports ─────────────────────────
  {
    id: 'm-t019', seq: 19, conversationId: 'c-ted', authorId: ME,
    at: at(3, '14:24'), state: 'failed',
    error: 'Send failed — server rejected upload (413)',
    attachments: [
      {
        kind: 'voice',
        durationSec: 22,
        peaks: peaksFromSeed(2281, 96),
        transcript: { state: 'pending' },
      },
    ],
  },

  // ── DM: Rosa ───────────────────────────────────────────────────────────
  {
    id: 'm-r064', seq: 64, conversationId: 'c-rosa', authorId: 'u-rosa',
    at: at(5, '19:30'), state: 'delivered',
    text: 'that’s the one, thank you',
  },
]
