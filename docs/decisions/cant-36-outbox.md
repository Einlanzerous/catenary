# CANT-36 — the outbox: a separate tail, its own database, held rather than failed

> **The Switchyard plan is the decision of record.** CANT-36 is Mode B and gated on one: **plan rev 3, approved 2026-09-29**, with all seven rulings picked by a person. This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale. Every "why" below is one sentence at most; the arguments are in the plan.

These are rules for **two** implementations: the TypeScript outbox (**CANT-161**, core; **CANT-162**, attachments; **CANT-163**, the transport adapter) and the Dart outbox (**CANT-42**), the second half of the client D3 split in two. They are recorded verbatim on CANT-42. Where they disagree with this file, this file is wrong and should be corrected rather than reinterpreted.

## What was picked

| ruling | outcome |
|---|---|
| where an unacked message lives | **a separate outbox**, keyed by `clientId`, rendered as a tail after the conversation's log — never pushed into the seq-sorted list |
| what persists it across a reload | **IndexedDB, in its own database** (`catenary-outbox`), not CANT-35's `catenary` database; Blobs held inline with the entry |
| does a surviving `pending` entry resend itself | **yes** — every `pending` entry resends on the next `ready`, including one that was in flight when the page died |
| what a retryable refusal does | **hold**: stay `pending` under capped backoff, and never reach `failed` from a retryable refusal alone |
| with two tabs open, who sends | **one drainer** — a Web Lock, held only while that tab's own transport session is `ready` |
| may an attachment be composed offline | **no** — the canvas stays as drawn, `ATTACH` dimmed offline, `RECORD` allowed |
| does the outbox enforce composed order | **no** — pipeline, and accept that a held entry is overtaken |

Built by **CANT-161** (core: store, state machine, rail projection, `store.ts`/`smoke.ts` rewrite, `NullTransport`), **CANT-162** (attachments), **CANT-163** (the transport adapter), and **CANT-42** (the Dart outbox, from this record).

## 1 · Where an unacked message lives, and its shape

A separate collection, keyed by `clientId` — never a row in `state.messages`, and never given a `seq` or `logSeq` of its own. The thread renders it after the conversation's seq-sorted log, ordered by `order`; an acked entry renders at `ack.seq` until the server's own record replaces it. The rail's `lastMessageOf` and `byRecency` merge the tail — the newer of a conversation's last server record and its newest unsettled entry — so a send updates a room's preview and position; `newCount` and `unreadCount` read server records only and are unaffected by anything in the outbox.

```ts
/** Persisted in database `catenary-outbox`, store `outbox`. keyPath clientId. Index: by_account_order [accountId, order]. */
interface OutboxEntry {
  v: 1                                   // record shape version
  clientId: Uuid                         // THE wire idempotency key; minted once at compose; never re-minted
  accountId: Uuid                        // author it is sent as; other accounts' entries are never sent
  conversationId: Uuid                   // what the send names; ack.conversationId wins once acked
  order: number                          // per-device, drawn in the put's own transaction; drain order. NOT the device clock
  composedAt: string                     // device clock; local display and rail ordering only; never sent, never the message's time
  text?: string
  replyToMessageId?: Uuid                // the only reply field sent
  replyPreview?: ReplyRef                // display-only; never sent
  attachments?: OutboundAttachmentDraft[]
  status: 'pending' | 'failed'           // sending vs queued is derived, never stored; there is no stored ack
  attempts: number                       // frames written for this entry
  internalRetries: number                // retryable refusals received; one per refusal, not per tab
  reuploads: number                      // bounded at 1
  notBefore?: string                     // earliest resend under backoff / retryAfterSec (wall clock)
  lastError?: { kind: 'server'; code: ErrorCode; message: string; retryable: boolean }
            | { kind: 'bare_1008' }
            | { kind: 'upload'; message: string }
}
```

## 2 · Its own database, strict durability, `order` inside the transaction

**Database `catenary-outbox`, owned by this outbox alone** — one object store, `outbox`. It is not a store in CANT-35's `catenary` database: a separate database means CANT-35 can `deleteDatabase('catenary')` (CANT-24 obligation 4's discard-and-bootstrap) without reaching an unsent message, and it removes any upgrade-lineage coupling between the two tickets' stores. The outbox never opens CANT-35's database and CANT-35 never opens this one.

Every transaction that writes an entry is opened `{ durability: 'strict' }`, and an operation resolves only after its `complete` event — the default is relaxed in Chromium, where `complete` means visible to other transactions, not flushed to disk, and a row the UI shows QUEUED must survive a crash just after compose. An entry is written durably, in one such transaction, **before it renders and before any `ClientSend` frame is written for it** (CANT-24 obligation 1's *persist before render*, applied to an entry).

`order` is allocated inside that same transaction: a cursor on `by_account_order`, highest value for the account plus one, then the `put`. IndexedDB serializes overlapping readwrite transactions on one store, so two contexts composing at once draw distinct values; allocating outside the transaction could let them collide.

## 3 · Derived, never stored

`status` is `pending | failed` only — no stored ack, and no stored `sending` / `queued` distinction. `sending` = `status === 'pending' && inFlight.has(clientId)`; `queued` = `status === 'pending' && !inFlight.has(clientId)`; `sent` = an in-memory ack is held for `clientId` this session; RETRYING (ruling 3, §6 below) = `status === 'pending' && internalRetries >= 3`. A reload empties the in-flight set and the in-memory acks, so every stored `pending` entry reads QUEUED until the drain sends it — a crash mid-send cannot leave an entry claiming `sending` or `sent` forever.

**No ack is persisted.** A persisted ack with no matching record would be a state the design has to exit — a restore behind the ack, a bootstrap regrowing a log without it — and would render SENT indefinitely. Not persisting it costs one duplicate frame after a reload for an entry that was acked but not yet settled, and the server's dedup turns it into `ack{duplicate: true}`.

## 4 · Settling an entry

A `message` frame or a `/sync` record whose `clientId` matches an entry **deletes that entry** — whatever its status: `pending`, in flight, acked, or `failed`. The server's record is now the only copy, with the server's `ReplyRef` and `at`, and renders as `sent`/`read` as served; a message the author wrote never renders `delivered`. This is why DELETE on a `failed` entry is safe (§7): it removes only the local copy, and if the server did hold the message, its record settles the entry the moment it arrives, from any status.

## 5 · Resend on reload and reconnect

Every `pending` entry — including one that was in flight when the page died — is written again under the **same** `clientId` once a session is next `ready`. `failed` entries never resend by themselves (§7). A socket that closes before an entry's ack, other than a bare `1008` (CANT-31 §7), leaves the entry `pending`; it is not an error and is not counted toward any budget.

## 6 · Retryable refusals — held, never failed

The non-retryable rows are fixed, independent of the ruling: `internal` with `retryable: false`, `not_a_member`, `conversation_not_found`, `message_too_large`, `unauthorized`, `wire_version_unsupported`, and the `unknown` sentinel all go to `failed` at once with the server's `message` as the inline error (RETRY stays offered — `failed` is never terminal).

The retryable rows — `rate_limited` and `internal` with `retryable: true` — **hold**: both keep the entry `pending` and resend under capped backoff (2 s, doubling, capped at 5 min, full jitter; `retryAfterSec`, when present, is a floor, never shortened). After 3 refused attempts the row carries a **RETRYING** label, derived from `internalRetries`, not stored or sent. **A `retryable: true` refusal never reaches `failed`** — only a non-retryable refusal or a bare `1008` does. Attempt counters are per entry, incremented once per refusal received, not once per context or per tab, and a resend is written only by the drain holder (§9).

`rate_limited` has no producer today — this is specified so the client is correct the moment one appears.

## 7 · `failed` is reachable and recoverable, never terminal

RETRY returns a `failed` entry to `pending` under the **same** `clientId`, clearing `lastError`; it rejoins the drain at its original `order`. DELETE is offered **only** on `failed`, and removes the local copy alone (§4). A `failed` entry is never resent without a RETRY. A `send` in flight when the session closes bare `1008` goes to `failed` (`lastError.kind: 'bare_1008'`) and is **not** requeued (CANT-31 §7); a send in flight at any other close, including a frame-preceded `1008`, stays `pending` and resends on the next `ready`.

## 8 · Multiple tabs — one drainer, held only while ready

Exactly one context drains: the holder of a Web Lock, `catenary.outbox`. A context requests the lock only while its own transport session is `ready`, and releases it when that session stops being ready (close, backoff, terminal) or the tab closes — the same primitive CANT-31 §2 uses for the credential. Every tab writes new entries to the shared database and posts on `BroadcastChannel('catenary.outbox')`; every tab re-reads on a post, so all tabs render the same entries. When the holder drops mid-drain, its in-flight entries are simply unacked-and-pending from the next holder's view, and resend under the same `clientId`.

## 9 · The `persist()` refusal is surfaced, not logged

`navigator.storage.persist()` is requested once, at the first compose. A refusal, or an unavailable API, is surfaced: while persistence is not granted and at least one unsent entry (`pending` or `failed`) exists, the thread's outbox tail carries one standing line that unsent messages are kept in this browser and may be cleared if storage runs low. Never a toast, never log-only — a QUEUED row implies the message will go out, and the client must not imply more durability than the browser has granted.

## 10 · Attachments — the canvas unchanged, a refusing default

**The canvas is unchanged**: `ATTACH` stays dimmed offline, `RECORD` stays allowed. No attach or capture UI exists yet to lift a dim from; the question is revisited when `CANT-247` builds a real `Uploader`.

Until `CANT-247` wires a real one, the production default is a `RefusingUploader` that rejects at once with a clear message ("Attachments can't be sent yet"), and an attachment entry goes to `failed` with that inline error — never a loop.

1. **No head-of-line blocking.** The upload queue and the send queue are independent; an entry with any attachment not yet uploaded is skipped by the send drain, not waited on, and joins the drain at its `order` once its last upload completes.
2. **Order is the cost of 1, accepted explicitly** (§11).
3. **A stale handle re-uploads once.** `upload_not_found` naming an entry's `clientId` clears the `uploadId`, re-uploads the held Blob, and resends under the same `clientId` — **once**. A second `upload_not_found` for a handle minted since the first goes to `failed`.
4. **RETRY after an upload failure uploads afresh.** A `failed` entry whose `lastError` is `upload_not_found` or an upload refusal has its `uploadId`s cleared on RETRY and is offered to the `Uploader` again under the same `clientId`. `reuploads` is not reset: the automatic re-upload happens once in an entry's life, and every later attempt is a person's.
5. **An upload is the drain holder's, on a ready session.** An attachment is offered to the `Uploader` only by the context holding `catenary.outbox` while its session is `ready`, once, and not again while that offer is unanswered; bounding an upload is the `Uploader`'s.

The attachment's Blob is held in the outbox database with the entry, which is what lets an attachment entry survive a reload and lets a stale handle be re-uploaded rather than lost.

## 11 · Composed order is not enforced

Pending text entries are pipelined in `order` over the one socket, without waiting for each ack; the server's sequential `readLoop` commits them in composed order when each succeeds first time. **An entry held back by a retryable refusal or an incomplete upload is overtaken**, and its server `seq` lands after its successors — accepted explicitly, not discovered. The thread re-sorts to the server's `seq` on ack, so the person sees the reordering once, at ack, not later.

## Stated here, built elsewhere

- **The TypeScript outbox core, rail projection, and `store.ts`/`smoke.ts` rewrite** — CANT-161.
- **Attachments in the outbox** — CANT-162, built against a fake `Uploader` with the refusing default.
- **The `OutboxTransport` adapter over CANT-35's `send` / `subscribe()` / `onSessionEnd` / `onApply`** — CANT-163, gated on CANT-35.
- **The Dart outbox and SQLite local store** — CANT-42, planned from this record.
- **Cross-client convergence**, the only mechanical check that the two outboxes agree — CANT-46.
- **CANT-31 §7** (session closed bare `1008` vs. frame-preceded) is consumed here, not reclassified; the classification of a close as bare or frame-preceded is CANT-35's.
- **A rate limiter** — unowned; `rate_limited` stays unemitted, and §6 only makes the client correct once one appears.
- **The presigned upload protocol and the uploads table** — CANT-48/CANT-47. **The real `Uploader` on both clients, the attach picker and voice capture** — `CANT-247`.
