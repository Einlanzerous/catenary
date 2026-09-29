/* CANT-35 criterion 15's type-level half: the shapes CANT-36's outbox adapter,
 * CANT-37 and CANT-39 compile against, pinned so that changing one fails
 * `vue-tsc --noEmit` — which `npm run build` and `./verify.sh` both run. Never
 * executed; it only has to compile. Mirrors no internal/client file: it pins
 * the TypeScript contract the plan's *Public surface* section states.
 *
 * `Exact<A, B>` is true only when A and B are mutually assignable, so a field
 * added, removed, renamed, made optional or retyped on either side is an error.
 */

import type {
  ClientRead,
  ClientSend,
  ClientTyping,
  Conversation,
  Message,
  ServerAck,
  ServerError,
  ServerReceipt,
  User,
} from '@/wire/generated'
import type { Applied, JournalSnapshot, SessionEnd, Transport, TransportConfig, TransportStatus } from '../index'
import { NotConnected, SendInFlight, SendRefused, SessionEnded, createTransport } from '../index'

type Exact<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false
const pin = <T extends true>(): T => true as T

pin<
  Exact<
    SessionEnd,
    {
      sessionGen: number
      opened: boolean
      readied: boolean
      closeCode: number | null
      preceding: ServerError | null
      bare1008: boolean
      verdict: 'reconnect' | 'reconnect_at_maximum' | 'terminal_protocol'
    }
  >
>()

pin<
  Exact<
    Applied,
    {
      source: 'page' | 'live' | 'wipe'
      cursor: number | null
      messages: Message[]
      conversations: Conversation[]
      users: User[]
      receipts: ServerReceipt[]
      wiped: boolean
    }
  >
>()

pin<Exact<JournalSnapshot, { cursor: number | null; messages: Message[]; conversations: Conversation[]; users: User[] }>>()

pin<Exact<Parameters<Transport['send']>, [ClientSend]>>()
pin<Exact<ReturnType<Transport['send']>, Promise<ServerAck>>>()
pin<Exact<Parameters<Transport['read']>, [ClientRead]>>()
pin<Exact<Parameters<Transport['typing']>, [ClientTyping]>>()
pin<Exact<Parameters<Transport['onSessionEnd']>, [(e: SessionEnd) => void]>>()
pin<Exact<Parameters<Transport['onApply']>, [(e: Applied) => void]>>()
pin<Exact<Parameters<Transport['subscribe']>, [(s: TransportStatus) => void]>>()
pin<Exact<ReturnType<Transport['snapshot']>, JournalSnapshot>>()
pin<Exact<ReturnType<Transport['status']>, TransportStatus>>()
pin<Exact<ReturnType<Transport['refreshIfDue']>, Promise<void>>>()
pin<Exact<ReturnType<Transport['start']>, void>>()
pin<Exact<ReturnType<Transport['stop']>, void>>()
pin<Exact<ReturnType<Transport['catchUp']>, void>>()
pin<Exact<ReturnType<Transport['retryNow']>, void>>()
pin<Exact<Parameters<typeof createTransport>, [TransportConfig]>>()
pin<Exact<ReturnType<typeof createTransport>, Transport>>()

// The four send refusals are classes a caller can `instanceof`.
pin<Exact<InstanceType<typeof SendRefused>['frame'], ServerError>>()
export const refusals = [NotConnected, SessionEnded, SendRefused, SendInFlight] as const

// `ready` is a field of every status `subscribe()` delivers.
pin<Exact<TransportStatus['ready'], boolean>>()
pin<Exact<TransportStatus['terminal']['kind'], 'none' | 'credential' | 'protocol'>>()
