# CANT-24 — resume-from-cursor: the socket does not resume, `/sync` does

> **The Switchyard plan is the decision of record.** It is versioned, its criteria carry verdicts and its rulings carry picks, and it gates the ticket: **Switchyard `CANT-24`, plan rev 2, approved 2026-09-12** (Switchyard is estate-internal, so there is no link that resolves from a clone of this public repository). This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale.

No migration. No wire shape change — eight `description` strings in `schema/catenary.wire.v1.schema.json`, regenerated into three languages, and the vector count stays 48. One store function, `internal/store/hello.go`. The socket half is `CANT-102`.

## Rulings, as settled

| | settled |
|---|---|
| **0 · what a socket resume may carry** | **Nothing — the server never streams.** Every hello is answered `ready{resumed: false, log_seq: head}` at once and the client catches up over `/sync` from its cursor. `resume_from_log_seq` stays on the wire as what the client holds and is logged against head; `resumed` stays as a field the server does not set in wire version 1, and a client that treats `true` as `false` is always correct. |
| **1 · the stream bound** | Moot — gated on ruling 0 streaming. |
| **2 · where `ready` sits relative to a backlog** | Moot. `ready` is the first and only frame a hello produces. |
| **3 · one signal or two when a hello cannot be streamed** | Moot in form, settled in effect: a hello is answered by `ready` alone. `resync_required` never answers a hello. |
| **4 · what an instance does on a NOTIFY gap** | Moot in form, settled in effect: `resync_required{cursor_too_old, log_seq: head}` to every attached session, no socket closed. The client's cursor is its last `/sync` high water, so its catch-up covers the gap by construction. |
| **5 · what moves the client's cursor** | **Only a `/sync` page's `log_seq`.** Live frames never move it. (`ready.log_seq` would, on a `ready` with `resumed: true`, which the server does not send.) |

## What a hello does

The server authenticates (CANT-22), attaches the session to the hub, and sends `ready{resumed: false, log_seq: head}`. Frames sent before `ready` are accepted and answered; only the server's heartbeat clock waits for `ready`. The client may issue its first `/sync` concurrently with the upgrade.

The one thing the server does with the cursor is compare it to head and log one structured line — `device_id`, `session_id`, `cursor` (absent if none), `head`, `delta`, `outcome` — at INFO, or at WARN for `cursor_ahead`. The outcomes are `no_cursor` · `behind` · `at_head` · `cursor_ahead`. The `delta` histogram is the only evidence that could reopen ruling 0; CANT-27's harness reports it.

## The five client obligations

Implemented twice — CANT-35 (TypeScript) and CANT-42 (Dart) — and checked by CANT-46. Cite them by number.

1. **Persist before render.** The cursor is written durably before any message it covers is shown or counted.
2. **The cursor moves on a `/sync` page's `log_seq` and on nothing else.** Live frames never move it. Messages deduplicate by **id**, never by `log_seq`; a later record for an id already held replaces it (a CANT-92 re-emission is exactly that). Every move is monotonic.
3. **Catch-up is re-entrant and ends on a page it asked for after the last trigger.** Triggers: a reconnect, `ready{resumed: false}`, `resync_required`. Live frames during catch-up are applied and rendered and do not move the cursor. Catch-up ends at the first page reporting `has_more: false` **whose request was issued after the most recent trigger**.
4. **A `ready.log_seq` below the client's cursor means discard and bootstrap.** Wipe messages, conversations, users and the cursor, then `/sync` from 0. Not merely re-sync: under obligation 2 a kept store would drop every regrown message as a duplicate.
5. **A live client learns conversation and user changes at its next catch-up, and not before.** The client may schedule a catch-up on its own policy to shorten that (CANT-35's call); obligation 2 makes any such catch-up correct.

A client does not derive whether to stream from `ready.log_seq`, and does not mint a fresh `client_id` on retry across a reconnect (CANT-36).

## What is open, stated so it does not read as closed

- **`cursor_ahead` catches only the window before the log regrows past the cursor.** Restore to head 80, thirty messages land, and a client at cursor 100 sees head 110 — `behind`, with different messages under the same ordinals, and nothing here can tell. The fix is a log generation id minted at migration and carried on `ready` and `SyncResponse`; that is a wire change owned by **CANT-68**'s restore drill and **CANT-74**'s policy.
- **The socket cannot introduce a conversation, and a live client is stale on conversations and users until its next catch-up.** Stated on the wire in `SyncResponse`. The fix is a `conversation` and a `user` frame on `ServerFrame` (CANT-89 ruling 1 option 2's frame, generalized) or a per-conversation `GET`; either is a wire change under **CANT-74**, and the decision is **CANT-103**.
- **`membership_changed` and `retention_purge` have no producer.** **CANT-75** and **CANT-67** respectively.

## What CANT-102 inherits

At hello: attach, `ready{resumed: false, log_seq: head}` at once, accept and answer frames before `ready`, start the heartbeat clock at `ready`. On `OnGap`: one `resync_required{cursor_too_old, log_seq: head}` to every attached session, one WARN per instance carrying `sessions_notified` and `head`, close nothing. The hub passes re-emissions and `receipt`/`typing`/`ack` frames through untouched. The kill test is R1's five phases in-process with a client implementing the five obligations, `/sync` issued concurrently with the upgrade, a listener gap planted while a `/sync` is in flight, and the discard-and-bootstrap case against a truncated-and-regrown log. The hub itself is unowned on the board and posted to CANT-22 to price or split.
