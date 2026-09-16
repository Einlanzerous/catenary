# CANT-103 — introducing a conversation to a live session: `conversation` and `user` frames gated on `seq == 1`

> **The Switchyard plan is the decision of record.** It is versioned, its criteria carry verdicts and its rulings carry picks, and it gates the ticket: **Switchyard `CANT-103`, plan rev 3, approved 2026-09-16** (Switchyard is estate-internal, so there is no link that resolves from a clone of this public repository). This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale.

No migration. Two additive wire types under CANT-74's compatibility policy — `x-wire-version` stays 1. No per-session state. The client obligations are CANT-24's, amended alongside this record; the two client tickets are CANT-35 and CANT-42.

## Rulings, as settled

| | settled |
|---|---|
| **0 · which shape introduces a conversation** | **E — a `conversation` frame and its members' `user` frames on `ServerFrame`, emitted when the fan-out message is that conversation's first (`seq == 1`).** `conversations.last_seq` starts at 0, has exactly one production writer, and that writer only increments it inside the inserting transaction, so the gate is a total, stateless test. |
| **1 · where per-session state lives** | **Moot.** E holds none. |
| **2 · does the socket carry freshness after the introduction** | **No.** Freshness for a rename, a membership change and `muted` stays at the client's next catch-up. The trigger for revisiting it is named: the ticket that writes the first such mutation, which is also the ticket that must extend E's `seq == 1` trigger for a member added to an *existing* conversation. |
| **3 · does this give `ResyncReason.membership_changed` a producer** | **No.** The schema sentence is corrected to name the real owner: no membership mutation exists yet, and the first one written is forced through `metadataBump` by CANT-91's guard, which owns the reason then. `retention_purge` remains CANT-67's. |

## What E does

`ServerConversationFrame{type, conversation}` and `ServerUserFrame{type, user}` — the shape `ServerMessageFrame` already has for `Message`. Emitted from `Hub.OnNotify`, immediately before the triggering `message` frame is enqueued onto the same session's outbox, in the order `conversation` → `user`(s) → `message`. The gate is `fm.Message.Seq == 1`, a field the fan-out already holds before any lock is taken; when true, the per-viewer conversation row and the members' display names are read inside `MessageForFanout`'s existing REPEATABLE READ snapshot, so the read never runs under `h.mu` and the steady state pays nothing. A failed load ends in the existing per-session gap; there is no new failure path, because there is no per-session state to fail out of. A session that already holds the conversation receives the introduction anyway and applies it idempotently by id.

## The two corrections from the rev 3 review

1. **`conversations.last_seq` is monotone except across a restore.** It has one production writer — `UPDATE conversations SET last_seq = last_seq + 1 ... RETURNING last_seq` (`internal/store/messages.go:499`) — inside the inserting transaction, and nothing else in the repository writes it, except a restore: `cmd/catenary/restore_test.go:60` rewinds it along with `log_counter`, and CANT-68's drill does the same against real hardware. That is benign under E — a post-restore first message re-introduces the conversation, which is exactly what a client needs once CANT-24 obligation 4 has made it discard and bootstrap. **CANT-67's retention sweep deletes message rows and must never touch `last_seq`** — density already forbade this, and E now depends on it too.
2. **`conversation_members.joined_at_seq`** is the expected extension shape for whoever writes the first membership mutation: the conversation's `last_seq` at the moment of the insert, existing rows backfilled at 0, drawn inside the membership insert's own transaction. `msg.seq == joined_at_seq + 1` is then the total test for *the first message this member should be introduced by* — it generalizes E per recipient rather than replacing it, and keeps the hub stateless. Recorded here for that ticket to accept or argue with, not to inherit blind.

## What is open, stated so it does not read as closed

- **Freshness of conversation and user records over the socket** — a rename, a membership change, `muted`. Deferred by ruling 2; lands on the ticket that writes the first such mutation, together with the `joined_at_seq` extension above.
- **`ResyncReason.retention_purge`.** Still unproduced, still CANT-67's.
- **Whether a client may advance its own unread marker from a receipt carrying its own `user_id`.** Raised here, left to CANT-35 and CANT-42. A receipt is live only per instance (CANT-111): a same-instance device gets it live, and a device attached to a different instance still learns of it at its next `/sync`.
- **`NotifyPayload`'s shape** is CANT-92's, decided separately.

## Where this is built

- **CANT-111** (merged, PR #56, `93bb636`) corrected the three wire-text clauses this decision falsified.
- **CANT-113** adds the two frames and their conformance vectors.
- **CANT-114** emits them from the hub, gated on `seq == 1` on a message notification, and never on a CANT-92 re-emission.
- **CANT-112** (this record) amends `docs/decisions/cant-24-resume.md` and carries the client rules onto CANT-35 and CANT-42.
