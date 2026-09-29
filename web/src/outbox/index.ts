/* The outbox (CANT-36). Rules: `docs/decisions/cant-36-outbox.md`. */

export * from './types'
export {
  Outbox,
  backoffMs,
  frameOf,
  isHeldRefusal,
  realClock,
  BACKOFF_BASE_MS,
  BACKOFF_CAP_MS,
  BARE_1008_MESSAGE,
  RETRYING_AFTER,
} from './outbox'
export type { OutboxOptions } from './outbox'
export { MemoryOutboxStore } from './memory-store'
export { IdbOutboxStore, OUTBOX_DB } from './idb-store'
export {
  BroadcastOutboxChannel,
  InProcessChannelHub,
  InProcessLockHub,
  WebLockDrainLock,
  CHANNEL_NAME,
  LOCK_NAME,
} from './coordination'
export { NullTransport, ScriptedServer, ScriptedTransport } from './transports'
export { TransportOutbox } from './transport-adapter'
export { errorText, project } from './projection'
