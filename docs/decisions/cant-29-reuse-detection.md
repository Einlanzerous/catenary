# CANT-29 — reuse detection: what counts as a replay, and what a replay costs

> **This file is the record, not a stub.** CANT-28's equivalent is derived from a Switchyard plan because that ticket is Mode B and gated on one. CANT-29 is **Mode C** — no plan gate, a human reads every line — so the decision below was taken in the ticket and is written here because nothing else would hold it. The choice was put to a person and picked on **2026-09-17**.

One migration, `0008_reuse_detection` (two indexes, no columns). One store file, `internal/store/refresh.go`. No wire change, no new route, no new table.

## The problem, stated exactly

CANT-97 made rotation single-use: presenting a spent refresh token fails the conditional update and returns zero rows. It is tempting to call that the replay signal, and CANT-28's record and this repository's comments both did. **It is a superset.** Three different things land on a spent row with `replaced_by` set:

| | what it is | how often |
|---|---|---|
| **a replay** | a copied token presented by somebody who should not have it | the incident this ticket exists for |
| **a lost race** | a waking phone with two in-flight refreshes; the loser presents a token the winner already spent | ordinary, and CANT-97's own concurrency test is this shape |
| **a lost response** | the server rotated, the response never arrived, the client retries | ordinary on a mobile network |

Nothing in the row separates them. Invalidating on all three logs a real person out every time their phone wakes up and reports an incident that did not happen — the failure **CANT-97's criterion 1** names, carried there verbatim from CANT-28's plan: *"a legitimate double-refresh invalidates a real person's family and forces a re-authentication that looks exactly like the incident it is meant to report"*. (Attribution corrected in review: the sentence is CANT-97's, not CANT-29's, and had been propagating as the latter since CANT-97.)

## The decision: a short grace window

**A presentation of a spent token within `ReuseGraceWindow` of the rotation that spent it is an echo. Outside it, a replay.** `ReuseGraceWindow` is **10 seconds**, a constant rather than config, on exactly the terms CANT-28's three lifetimes are: a security posture a human picked, where an environment variable would be a way to widen it without anybody re-reading the argument.

**Time is the only distinguisher available, and it is a good one.** A race is milliseconds; a working device's next refresh is fifteen minutes away, so a theft's victim presents their stale token three orders of magnitude outside the window.

**The rotation is dated from the successor's `issued_at`, and there is no new column.** `refresh_tokens.replaced_by` is `ON DELETE RESTRICT`, so a chain cannot be broken in the middle and the row a spent token names always exists — `internal/store/schema_test.go` already calls that constraint *"precisely the evidence CANT-29's reuse detection reads"*.

**The age is subtracted by Postgres, not by the application,** and that was a correction found in review. `issued_at` is stamped by the database (`DEFAULT now()` in 0007; `insertRefresh` does not write the column) while `ServerTime()` is `time.Now()` in the service, so subtracting one from the other made the whole discrimination a comparison across two hosts' clocks against a ten-second threshold. A database more than ten seconds ahead makes every age negative, every presentation an echo, and **the detector silently off** — with every test still green, because CI shares one clock between the runner and its Postgres service. `now()` is transaction-start time on both sides, so a slow winner spends part of the loser's budget; at milliseconds against ten seconds that is noise, and it is written down so it is not rediscovered as a bug.

**The invalidation is detached from the request that triggered it.** The context reaching this path is the caller's, and Go cancels it when the client's connection closes. The refusal is already decided by then, so cancellation cannot change what the caller is told — but it could kill the invalidation, letting somebody present a stolen token, hang up, and leave the family live at will. `context.WithoutCancel` with a bounded timeout, the shape `notify.go` already uses.

### The exposure this buys, stated rather than left to be found

An attacker who replays a stolen token **within ten seconds** of the victim's own rotation escapes family invalidation. They still receive a 401 and the spent token still buys them nothing; what they escape is the **detection**. That is the trade, and it is the right way round — the alternative fires on ordinary traffic, and a detector that cries wolf is one that gets turned off.

### Two alternatives, rejected with reasons

- **Strict — any reuse invalidates.** No constant to argue about and maximum detection. Rejected because CANT-97's concurrency case is a working phone, and this would invalidate its family every time it woke up.
- **Chain depth — invalidate only when the successor has itself been rotated.** No magic number, and superficially principled. Rejected because it **misses the primary threat**: an attacker who uses a stolen token *first* leaves the successor unrotated, so the victim's later presentation reads as benign and the attacker keeps the family. Recorded here so it is not rediscovered as an improvement.

## What a replay costs

Four writes and a notification, **in one transaction**, on `RevokeDevice`'s own argument — Postgres delivers a NOTIFY at commit, so a notification cannot exist without its cause or the reverse, and a partially-applied invalidation is the worst available state.

1. the whole family — `refresh_tokens.revoked_at`, one predicate over `family_id`;
2. the device's access tokens, because *"forced to re-authenticate rather than silently continuing"* is not delivered by a family-only revocation: the current access token would keep working for the rest of its fifteen minutes;
3. `devices.revoked_at`, which is what makes every other mechanism agree — `Authenticate` refuses it, `DeadDevices` names it, and CANT-30's gap re-check reaches the same verdict as the notification;
4. a `RevocationPayload` on `catenary_device_revoked`, so CANT-30 severs the live socket — which matters because under CANT-28 ruling 2 a session outlives the token that opened it.

**A family is one device's**, which is what makes the device write correct rather than collateral: a family begins at one enrollment and every rotation carries the same `device_id` forward.

**The invalidation runs after the rotation transaction has rolled back, in a transaction of its own.** The rotation holds an uncommitted successor row; invalidating inside it and committing would mint a brand-new live token into the family it had just revoked.

**Idempotence is gated on the device write, not on the `IS NULL` guards.** The guards make the writes idempotent and say nothing about the NOTIFY; publishing again would sever a device that was severed the first time, which is the opposite of what R6 asks. The device `UPDATE … RETURNING` is the gate, exactly as `RevokeDevice`'s is. A repeat replay is still logged — at WARN, as a repeat rather than a fresh incident — because somebody presenting a stolen token repeatedly is worth seeing.

## What "recorded" means

**A structured log line, and no new table.** ERROR for an invalidation — the only line in this service that says a token was copied — carrying `family_id`, `device_id`, `user_id` and the counts, and never the credential, which is the rule every credential log here follows. A durable security-events table was considered and declined: there is no audit-table precedent in this schema, and it would bring its own retention question. Visibility is what CANT-28 names as standing in for the rate limiter these routes deliberately do not have.

## The operational consequence, which is real

**There is no password login.** A device whose family is invalidated cannot recover on its own: it needs a fresh enrollment token, and those come from Purser. So a replay — genuine or, inside the exposure above, mistaken — costs the person an admin re-invite. That is the correct outcome for a stolen credential and it is a real cost, and it is the strongest argument for the window being a window rather than zero.

## Two indexes, and why neither is speculative

`0007` shipped `family_id` deliberately unindexed on the rule that *"the COLUMN has to be here because adding it later rewrites the table; the INDEX does not, so it belongs to the ticket whose query needs it."* This is that ticket, and it adds two:

- `refresh_tokens (family_id)` — the invalidation predicate.
- `access_tokens (device_id)` — the access revocation above. That table had no index on `device_id` at all, and CANT-118 records it growing by roughly 96 rows per device per day with nothing sweeping it yet, so an unindexed scan there would rot quietly.

## What the clients inherit

**Nothing new on the wire.** No frame, no field, no vector. A device whose family was invalidated meets the ordinary 401 on `POST /refresh` and the ordinary refusal at the socket door, and its live socket ends with `4001` — the terminal close code CANT-30 added and recorded on CANT-35 and CANT-42. The client rule is the one already written there: **do not reconnect; the credential is gone.**

## It clears the deploy bar

`deploy/README.md` holds the `public` entrypoint against three tickets — CANT-22, CANT-28 and CANT-29 — because *"a rotating refresh token whose replay is merely refused, rather than invalidating the whole family, leaves an attacker who rotated first holding live credentials while the real device's refusal looks like an ordinary bug."* This is the third. Whether to stand a public router up is still an operator's decision; it is no longer blocked on this.
