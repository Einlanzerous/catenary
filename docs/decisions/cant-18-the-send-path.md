# CANT-18 — the send path above the insert: validation, attachments, and the `Message` both transports return

> **The Switchyard plan is the decision of record.** It is versioned, its criteria carry verdicts and its rulings carry picks, and it gates the ticket: **Switchyard `CANT-18`, plan rev 2, approved** (Switchyard is estate-internal, so there is no link that resolves from a clone of this public repository). This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale.

Five sub-tasks: `CANT-82` (the wire package's home), `CANT-83` (validation and the error taxonomy), `CANT-84` (the wire mapping), `CANT-85` (attachments), `CANT-86` (`pg_notify`). No migration: CANT-13 landed every column this needed.

## Rulings, as settled

| | settled |
|---|---|
| **1 · resolving `upload_id`** | **Resolver seam now, no E6 schema.** The lookup is behind an interface this ticket defines; CANT-48 mints the handles under CANT-47's storage decision. |
| **2 · where `pg_notify` is called** | **Inside `SendMessage`'s transaction.** Postgres delivers at commit, so a notification cannot exist without its message or the reverse. CANT-21 owns everything downstream of the call. |
| **3 · where the generated Go wire package lives** | **`internal/wire`, in the service module.** Go applies the internal rule to the import path, so no module wiring made `server/internal/wire` reachable. The `server` module now requires the service module with a relative `replace`. |
| **4 · a `reply_to` that does not resolve** | **Store NULL — the ref is dropped, the send succeeds.** |

## The send path, in order

`Store.SendMessage`, `internal/store/messages.go`. The order is the design.

1. `client_id` present — before the pool is touched.
2. Size and attachment count — before the pool is touched.
3. Begin.
4. Idempotency check — before either ordinal is drawn.
5. Membership and existence — one query, two `EXISTS`.
7. Upload resolution (`CANT-85`).
8. Draw `seq` from `conversations.last_seq`.
8b. `reply_to` resolution, `FOR KEY SHARE`.
9. *(empty — the author's `read_seq` advance was removed; see below.)*
10. Draw `log_seq` from `log_counter`.
11. Insert the message, then its attachments (`CANT-85`).
12. `pg_notify`, inside the transaction and last before commit (`CANT-86`). The payload is CANT-21's `(conversation_id, seq)`.

**A replay wins over every refusal that depends on server state** — membership, existence, `reply_to`, upload resolution — because position 4 sits above all of them.

**It does not win over the two size bounds**, and that is an accepted cost: an operator who lowers `CATENARY_MAX_MESSAGE_BYTES` under an acked message makes that message's replay report `message_too_large`.

**`reply_to` resolution moved from position 6 to 8b**, below the conversation lock. Superseded on the field finding recorded in `CANT-83`: at position 6 the read took no lock, so a source deleted before position 11 made the insert raise `23503` — not a transient class — and the send failed permanently over a `reply_to` the sender cannot fix.

## The lock order is three, and it is still a deadlock rule

**`conversations` → `messages` → `log_counter`.** All row locks held until commit. CANT-14 established the outer two; CANT-83 added `messages` in the middle.

The insert at position 11 also takes `KEY SHARE` on `users(author_id)` and, when the sender has one, `devices(sender_device_id)` — `messages` has four foreign keys and an insert locks every row it references. Named because no path in this service takes a conflicting lock on either today, so a future one would be the first: `deactivated_at` and `revoked_at` are non-key updates and do not conflict, and nothing deletes from those tables outside the `RESTRICT` tests. `CANT-33` and `CANT-63` are the tickets most likely to change that.

`messages` is not new — position 11's FK check always took `KEY SHARE` on the `reply_to` source, after the counter draw. What changed is when: 8b takes the same lock earlier and explicitly. Taking it at position 6, above the conversation lock, would have inverted against CANT-67's sweep.

**The `messages` lock is only ever taken on a row in this conversation**, and the resolve is scoped to make that so. A table order cannot describe a per-row hazard: an unscoped resolve let a send in conversation A take `messages(X ∈ B)` for a ref it then discarded, closing a deadlock cycle in which both parties had obeyed `conversations` → `messages`. Scoped, the only row locked is one whose conversation this transaction already holds — which is also what makes "the lock is not new" true rather than nearly true.

**`conversation_members` is not taken at all**, and that is the point of removing position 9. It deletes a lock, the ordering obligation stated outward with it, and the deadlock class that came with both — out of the one transaction that can least afford any of the three.

- **CANT-67's** sweep advances a floor on `conversations` and then deletes from `messages` — the same direction as this file.
- **CANT-63** draws `log_counter` for an edit and takes these in this order.
- **CANT-26's** receipt write takes `conversation_members` and, since `CANT-89`, `log_counter` after it. The send path never locks a member row, so the counter is the only lock the two share, and both take it last: no cycle in either direction.

**The commit takes one more lock, and it is instance-wide.** `pg_notify` at position 12 locks nothing when it runs; at commit `PreCommit_Notify` takes an `AccessExclusiveLock` on "database 0", shared by every notifying committer on the Postgres instance, other services' databases included. It is acquired inside commit after every row lock and nothing waits on a row lock after it, so it is last in every notifier's order and cannot join a cycle. `RevokeDevice` is the other transaction in this service that takes it.

## What the operation guarantees

- Every refusal leaves the store as a `*SendError` carrying `wire.ErrorCode`, `retryable` and `retry_after_sec`. No transport decides a code.
- **Exactly one file decides a code** — `internal/store/senderror.go` — enforced by a guard test that bans the type, the six send-code constants and the six code *values* as string literals, anywhere else in the service module. `unauthorized` and `wire_version_unsupported` are exempt by name for CANT-22 and CANT-29.
- `internal` splits on transience: FATAL/PANIC severity, `40001`, `40P01`, class `08`, `53` or `57`, `pgconn.SafeToRetry`, a closed pool, a context error, or a connection-shaped error with no `PgError`. Everything else is permanent.
- A refusal **consumes no ordinals** — neither the conversation's dense `seq` nor the deployment-wide counter.
- **`Sent.ConversationID` is the conversation the row is in**, which is not always the one the caller asked about: dedup is `(author_id, client_id)`, not per conversation. Transports build the ack from `Sent`, never from the request.
- **A send does not touch `read_seq`.** `first_unread_seq` is derived as the first `seq` above `read_seq` the viewer did not author — an index scan over `UNIQUE (conversation_id, seq)`, not a table scan. `0005_read_seq_derivation` carries the correction into the column comment; CANT-26 owns the query.
- A `reply_to` that is missing or in another conversation is stored NULL and logged at `info` with both ids. The send succeeds, and the source is held `FOR KEY SHARE` so a concurrent delete cannot turn a valid ref into a failed send.
- **A membership revoked between position 5 and commit is not caught**, and that is accepted: one more message lands from someone who was a member when asked. Closing it means locking the member row at position 5, which takes it *before* `conversations` and inverts against CANT-67's sweep.
- An **empty send** — no `text`, no attachments — is stored.
- **A committed send raises exactly one notification, ids only, at commit.** A refusal, a replay, a race loser and a rolled-back send raise none. The call is inside the insert's transaction, on the same connection, last before commit; an over-cap payload (`ErrNotifyTooLarge`, unreachable with two fixed-width fields) fails the send as `internal`, not retryable.
- Every refusal **logs once**: `warn` for `internal`, `info` for the rest, with `conversation_id`, `author_id`, `client_id`, `code` and `retryable`; **every** `internal` also logs its SQLSTATE and constraint name, retryable or not, because the permanent ones are the ones that need diagnosing. **The body is never logged, at any level** — `pgErr.Detail` is excluded by name, because on a CHECK violation it renders as `Failing row contains (…)` and that row is the message.

## Rejected

| | why not, in one line |
|---|---|
| Deciding codes in the transports | The same cause acquires two codes on the path nobody looks at — Invariant 2's failure class on the error path. |
| A store-side error enum translated to the wire's | A translation table is the second place the two transports can disagree. |
| Collapsing `not_a_member` into `conversation_not_found` | Tells a member of a deleted conversation the wrong thing; the existence leak to a non-member is the accepted trade. |
| Refusing a `reply_to` that does not resolve | The schema and the wire already model "no ref" — `ON DELETE SET NULL`, optional `Message.reply_to` — and it fails a send over a field the sender cannot fix. |
| `FOR KEY SHARE` at position 6, above the conversation lock | Inverts against `CANT-67`'s sweep, which takes `conversations` then `messages`: trades a rare permanent failure for a routine deadlock. |
| A savepoint around the insert, catching `23503` and retrying with NULL | Recovers from the failure instead of preventing it, and puts a retry loop inside the one transaction `CANT-14` argues hardest for keeping small. |
| Refusing an empty send | Needs either a new `ErrorCode` — a wire change under CANT-74's unresolved compatibility policy — or reporting a client bug as `internal`. |
| Moving the size bounds below the idempotency check | Buys consistency for a rare, deliberate operator action at the price of a transaction per oversized frame. |
| `bigserial` for either ordinal | Unchanged from CANT-14: the number is handed out outside the transaction. |
| Advancing the author's own `read_seq` on send | Built, then removed. It made `first_unread_seq` arithmetic at the price of marking an unread backlog read whenever an author replied without opening the thread — and the arithmetic was never necessary, because the derivation is an index scan. It also put a fourth lock in this transaction. |
| Leaving `read_seq` alone *and* keeping `read_seq + 1` | The author's own message then counts toward their own unread, which Invariant 3 forbids outright. |

## Not claimed here

A **rate-limiting policy**. `rate_limited` and `retry_after_sec` are plumbed end to end and the vector stays satisfied, but what is limited, per what, and at what rate is not on the board and wants its own ticket.

**Link previews.** `ReplyRefKind` includes `link`, nothing derives it, and the server never emits it.
