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
6. `reply_to` resolution.
7. Upload resolution (`CANT-85`).
8. Draw `seq` from `conversations.last_seq`.
9. Advance the author's `read_seq`, `GREATEST(read_seq, $seq)`.
10. Draw `log_seq` from `log_counter`.
11. Insert the message, then its attachments (`CANT-85`).
12. `pg_notify` (`CANT-86`), then commit.

**A replay wins over every refusal that depends on server state** — membership, existence, `reply_to`, upload resolution — because position 4 sits above all of them.

**It does not win over the two size bounds**, and that is an accepted cost: an operator who lowers `CATENARY_MAX_MESSAGE_BYTES` under an acked message makes that message's replay report `message_too_large`.

## The lock order is three, and it is still a deadlock rule

**`conversations` → `conversation_members` → `log_counter`.** All row locks held until commit. CANT-14 established the outer two; position 9 added the middle one.

Stated outward, for the tickets that take a member row without sending: **never take the conversation row after a member row.**

- **CANT-26's** receipt write takes the member row alone, which is safe as a single lock. If it ever also touches the conversation, the conversation goes first.
- **CANT-67's** sweep advances a floor on `conversations` and must keep doing that before any member row it later needs.
- **CANT-63** draws `log_counter` for an edit and takes these in this order.

## What the operation guarantees

- Every refusal leaves the store as a `*SendError` carrying `wire.ErrorCode`, `retryable` and `retry_after_sec`. No transport decides a code.
- **Exactly one file decides a code** — `internal/store/senderror.go` — enforced by a guard test that bans the type, the six send-code constants and the six code *values* as string literals, anywhere else in the service module. `unauthorized` and `wire_version_unsupported` are exempt by name for CANT-22 and CANT-29.
- `internal` splits on transience: FATAL/PANIC severity, `40001`, `40P01`, class `08`, `53` or `57`, `pgconn.SafeToRetry`, a closed pool, a context error, or a connection-shaped error with no `PgError`. Everything else is permanent.
- A refusal **consumes no ordinals** — neither the conversation's dense `seq` nor the deployment-wide counter.
- **`Sent.ConversationID` is the conversation the row is in**, which is not always the one the caller asked about: dedup is `(author_id, client_id)`, not per conversation. Transports build the ack from `Sent`, never from the request.
- An author's own send advances their `read_seq` in the same transaction, as a floor. A replay advances nothing.
- A `reply_to` that is missing or in another conversation is stored NULL and logged at `info` with both ids. The send succeeds.
- An **empty send** — no `text`, no attachments — is stored.
- Every refusal **logs once**: `warn` for `internal`, `info` for the rest, with `conversation_id`, `author_id`, `client_id`, `code` and `retryable`; a retryable `internal` also logs its SQLSTATE. **The body is never logged, at any level.**

## Rejected

| | why not, in one line |
|---|---|
| Deciding codes in the transports | The same cause acquires two codes on the path nobody looks at — Invariant 2's failure class on the error path. |
| A store-side error enum translated to the wire's | A translation table is the second place the two transports can disagree. |
| Collapsing `not_a_member` into `conversation_not_found` | Tells a member of a deleted conversation the wrong thing; the existence leak to a non-member is the accepted trade. |
| Refusing a `reply_to` that does not resolve | The schema and the wire already model "no ref" — `ON DELETE SET NULL`, optional `Message.reply_to` — and it fails a send over a field the sender cannot fix. |
| Refusing an empty send | Needs either a new `ErrorCode` — a wire change under CANT-74's unresolved compatibility policy — or reporting a client bug as `internal`. |
| Moving the size bounds below the idempotency check | Buys consistency for a rare, deliberate operator action at the price of a transaction per oversized frame. |
| `bigserial` for either ordinal | Unchanged from CANT-14: the number is handed out outside the transaction. |

## Not claimed here

A **rate-limiting policy**. `rate_limited` and `retry_after_sec` are plumbed end to end and the vector stays satisfied, but what is limited, per what, and at what rate is not on the board and wants its own ticket.

**Link previews.** `ReplyRefKind` includes `link`, nothing derives it, and the server never emits it.
