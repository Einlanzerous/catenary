# CANT-14 — both ordinals, assigned inside the insert's own transaction

> **The Switchyard plan is the decision of record.** It is versioned, its criteria carry verdicts and its rulings carry picks, and it gates the ticket: **Switchyard `CANT-14`, plan rev 2** (Switchyard is estate-internal, so there is no link that resolves from a clone of this public repository). This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale.

One operation, `Store.SendMessage` in `internal/store/messages.go`. No migration: CANT-13 landed every column this needed.

## Rulings, as settled

| | settled |
|---|---|
| **1 · where the draw takes its lock** | The **counter row** — `UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`. Not `pg_advisory_xact_lock` + a sequence, which measured faster under contention. Picked on plan rev 2. |
| **2 · scope of the deletion** | The reference copy is **gone**, and all 16 former call sites drive the shipped primitive. `schema_test.go` no longer proves anything about a copy. |

## The lock order is a rule, and it is a deadlock rule

**`conversations.last_seq` FIRST, `log_counter` LAST.** Both are row locks held until commit.

Two writers taking the two in opposite orders deadlock, and Postgres resolves a deadlock by aborting somebody's send.

- **CANT-63 will draw this counter** — an edit bumps `updated_log_seq` — and must take the two in this order.
- **CANT-67 will not.** Its sweep advances `conversations.oldest_seq`, a floor, inside its own transaction. It never touches `log_counter`.

## What the operation guarantees

- Both ordinals are drawn **inside the inserting transaction**. No function in `package store` returns an ordinal without writing the row; `logorder_test.go`'s `beginDraw` is the one deliberate exception and is test-only.
- Idempotency is checked **before either ordinal is drawn**. On the residual race the unique violation is caught, rolled back — which un-draws both — and re-selected by key.
- A replay is **not an error**. `Sent.Duplicate` reports it and the original's ordinals come back with it.
- A nil `ClientID` means **no deduplication**, which is correct for a bot posting through CANT-75's REST send and for anything the server originates.
- `reply_to` is written by the send, not by a follow-up `UPDATE`.

## The throughput ceiling is deliberate

The counter row serialises message inserts deployment-wide: roughly **1000–1800 inserts/s**, not scaling past 8 concurrent writers, against ~12–13k/s for an incorrect bare sequence. At the 1–2 concurrent senders this deployment produces, ~1200/s with p99 under 2 ms.

Measurements and the full comparison against the advisory lock are in **CANT-19 plan rev 2**, `tradeoffs`. Recorded here so a later reader does not mistake the ceiling for an oversight.

**The worst case is unbounded on Postgres 16** — no cap on a transaction's total duration, so one slow holder blocks every message insert. `transaction_timeout` (Postgres 17) closes it per-role: **SERV-163**.

## Rejected

| | why not, in one line |
|---|---|
| `bigserial` / any sequence | Hands out its number outside the transaction; two inserters commit out of order and the lower `log_seq` is invisible to every sync that follows. |
| `uuidv7()` (Postgres 18) | Generated at **draw** time, so it fails exactly as a `bigserial` does. The attractive-looking wrong answer once the estate upgrades. |
| `bigserial` + `pg_snapshot_xmin` watermark | **Not sound.** The watermark bounds transaction ids and `nextval` assigns no xid, so the two are drawn under no common order. The sound form makes the ordinal *be* the xid (`pg_current_xact_id()` as `xid8`, served below the snapshot xmin) and carries a stall a shared cluster makes unacceptable. |
| Logical decoding / WAL LSN | Needs `wal_level = logical` and a restart of the estate's cluster; a wedged consumer takes down every service on it. |

## The guard

`internal/store/logorder_test.go`, from CANT-19, now runs against this primitive rather than a copy. Three things stand behind the invariant:

- **the structural guard** — zero sequences in `public`, no default or identity on either ordinal;
- **Arm 1's positive half** — proves the counter's row lock is taken, from `pg_stat_activity`;
- **Arm 2** — the one that catches a draw which leaves the inserting transaction. Proved on this diff: moving the shipped draw to the pool makes Arm 2 fail 5 runs out of 5.
