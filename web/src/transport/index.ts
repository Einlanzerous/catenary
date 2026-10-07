/* The transport's public surface. Mirrors internal/client's exported API
 * (`client.New`, `Client`, `Status`, `Stats`, `Snapshot`, `Faults`); the
 * modules below each name the Go file they mirror.
 *
 * CANT-36's outbox adapter compiles against `Transport`, `SessionEnd`,
 * `Applied` and `JournalSnapshot`; CANT-37 and CANT-39 against
 * `TransportStatus`, `connectionInfo`, `project` and `projectApplied`; CANT-152
 * against `CredentialSeam`. Those names are a contract (CANT-35's plan,
 * *Public surface*), pinned by `test/contract.typetest.ts`.
 */

export {
  createTransport,
  NotConnected,
  SendInFlight,
  SendRefused,
  SessionEnded,
  SUBPROTOCOL_V1,
  TOKEN_SUBPROTOCOL_PREFIX,
} from './transport'
export type { SessionEnd, Transport, TransportConfig } from './transport'
export { MemoryJournal, StagedJournal } from './journal'
export type { Applied, Journal, JournalSnapshot, LiveWrite, MemoryJournalOptions } from './journal'
export { IdbJournal, JournalStale, JournalWriteAborted } from './idb-journal'
export type { IdbJournalOptions } from './idb-journal'
export { connectionInfo, emptyStats } from './status'
export type { JournalError, Stats, TransportStatus } from './status'
export { EMPTY_PROJECTION, project, projectApplied } from './project'
export type { Projection } from './project'
export { heldCredential, isCatenaryUnauthorized } from './credential'
export type { Credential, CredentialHost, CredentialSeam, CredentialStatus, RefreshHold } from './credential'
export {
  createRefreshingCredential,
  mintProposal,
  RefreshingCredential,
  refreshDue,
  refreshThreshold,
  REFRESH_FLOOR_MS,
} from './refresh'
export type { DueInput, RefreshingCredentialConfig, RefreshOutcome } from './refresh'
export {
  CHAIN_WARN_LENGTH,
  gateOpen,
  nextRefreshAt,
  readStamp,
  refreshDelay,
  refreshHoldAt,
  REFRESH_BACKOFF_BASE_MS,
  REFRESH_BACKOFF_CAP_MS,
} from './hold'
export type { HoldInput } from './hold'
export { refusedHoldAt } from './refused'
export type { RefusedInput } from './refused'
export {
  credentialFromEnroll,
  credentialLockName,
  CredentialHeld,
  enrollCredential,
  IdbCredentialStore,
  MemoryCredentialStore,
  NoCredential,
  reenrollCredential,
} from './credential-store'
export type { ChainLink, CredentialStore, StoredCredential } from './credential-store'
export { EnrollAnswerUnreadable, enrollDevice, EnrollRefused } from './enroll'
export type { EnrollOptions } from './enroll'
export { CATENARY_DB, CREDENTIAL_STORE, openCatenaryDb, UPGRADES } from './db'
export type { OpenCatenaryDbOptions, UpgradeStep } from './db'
export { classifyClose, closeStatusKey, CLOSE_POLICY_VIOLATION, CLOSE_REVOKED } from './closes'
export type { CloseVerdict } from './closes'
export {
  advance,
  BACKOFF_MAX_MS,
  BACKOFF_MIN_MS,
  JITTER_FLOOR,
  jitteredWait,
  resetsRamp,
  unitFromBytes,
} from './backoff'
export { NO_FAULTS } from './faults'
export type { Faults, JournalFaults } from './faults'
export { NOT_TERMINAL } from './terminal'
export type { Terminal, TerminalKind } from './terminal'
export {
  browserLifecycle,
  browserLock,
  browserTimers,
  consoleLogger,
  cryptoRandom,
  inProcessLock,
  manualLifecycle,
  silentLogger,
} from './seams'
export type {
  Clock,
  Lifecycle,
  LifecycleEvent,
  Lock,
  Logger,
  RandomBytes,
  Timers,
  WebSocketCtor,
  WebSocketLike,
} from './seams'
