# CANT-31 — refreshing before you need to, and when a client must stop

> **The Switchyard plan is the decision of record.** CANT-31 is Mode B and gated on one: **plan rev 5, approved 2026-09-19**, with all six rulings picked by a person. This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale. Every "why" below is one sentence at most; the arguments are in the plan.

These are rules for **three** implementations: `internal/client` (the reference, and the one the soak and idle rigs drive), the TypeScript transport (**CANT-35**), and the Dart transport (**CANT-42**). They are recorded verbatim on both client tickets. Where they disagree with this file, this file is wrong and should be corrected rather than reinterpreted.

## What was picked

| ruling | outcome |
|---|---|
| where the rule lives | this record, plus the rules recorded identically on CANT-35 and CANT-42 |
| what stops a refresh costing an admin re-invite | **both** — single-flight in the clients, and a client-proposed successor on the server |
| the deliverable | a reference implementation in `internal/client`, plus this record |
| when to refresh | **proactive and reactive together**, on the wall clock corrected by the `Date` offset |
| more than `4001` on revocation | no — the close status is the whole signal |
| telling a permanent `1008` from a transient one | give the hello timeout its own private-range close code |

Built by **CANT-120** (this file), **CANT-121**, **CANT-122**, **CANT-123**, **CANT-124**, **CANT-125** and **CANT-126**.

## 1 · Refresh before you need to, and also when you are refused

**Proactive.** Refresh before reconnecting whenever less than `max(60 s, ⅓ of the served lifetime)` remains. Both clients use that same number; it is here so they cannot each pick one.

**The anchor is the wall clock, corrected by the `Date` offset.** Capture the offset between the HTTP `Date` header and the device wall clock on the `/refresh` and `/enroll` responses — at the moment the expiry is learned — and persist it with the credential. Use it to interpret `access_expires_at`. Never schedule on a monotonic clock: Go's stops on suspend, `performance.now()` is unspecified across browsers, and only Android's `elapsedRealtime()` counts sleep. In Go, `Round(0)` strips the monotonic reading. `ServerReady.server_time` is **not** the anchor — it arrives after the upgrade, which is after the decision that needs it.

**Three edges of that, settled by the reference client (CANT-124) so the other two do not each pick.** The *served lifetime* is measured on the server's clock at both ends — `access_expires_at` minus the `Date` of the response that served it — so a wrong device clock cannot stretch it. **A pair whose issue time was never learned gets the 60 s floor**, not a guessed lifetime. **A `/refresh` response with no usable `Date` keeps the offset the device already had**: the offset describes the device's clock, not one pair, and a stale correction is a better guess than none.

**Reactive, and not optional.** On any Catenary `/sync` 401: single-flight refresh, then retry **once** — not a loop. A browser `WebSocket` never exposes a failed upgrade's HTTP status (a 401 surfaces as `error` then `close 1006`), so the reactive path is the only way a TypeScript client can learn its token is stale, and it is the safety net for every error the proactive check makes.

## 2 · Single-flight, per credential — not per process

At most one refresh in flight **per credential**. Before refreshing, re-read the persisted credential under a lock every context holding it respects — Web Locks in a browser, a SQLite transaction on Android — and skip the refresh if another context has already rotated it. Concurrent callers await that result rather than starting their own.

An in-memory mutex is not enough: two browser tabs, or an app and its push worker, share a credential without sharing memory.

**Persist before use.** The rotated pair is durable before the new access token is presented anywhere. This is CANT-24 obligation 1's *persist before render*, applied to a credential.

## 3 · When the outcome of a refresh is unknown: walk the chain

A refresh whose response is lost leaves the client unable to tell whether the rotation committed. The rule:

- the client generates a successor secret **P** per attempt, as 32 CSPRNG bytes in the 43-character `Token` shape, never derived from anything, and persists it **with the token it is presented against** before sending;
- on an unknown outcome, present the **newest** token first, and walk back one step at a time;
- **whenever a token is presented again, reuse the proposal it was first presented with**, so a racing commit collides instead of forking the family;
- a **401 during the walk-back is not terminal**. Only a 401 on the *oldest* token is, subject to §5.

The persisted state therefore holds the chain, not a single pair. The response's `refresh_token` is **authoritative — never P**: a server that predates the field ignores the proposal and mints its own.

The server's half is CANT-125: a proposal colliding with a stored hash is routed on how the presented token relates to the colliding row, and only a genuine spent-token presentation reaches reuse detection.

## 4 · Terminal, by close code

| close | client does |
|---|---|
| `4001` | **terminal.** The credential behind the session was revoked. |
| `1008` **bare** | **terminal.** A bare close means a client bug — see the table below. |
| `1008` preceded by `error{unauthorized}` or `error{wire_version_unsupported}` | **terminal.** The two codes the door produces directly, and the two that are the client's own fault. |
| `1008` preceded by `error{internal}` | **reconnect, at maximum backoff.** `retryable` is not consulted. |
| `1008` preceded by an `error` carrying any other code | reconnect with backoff |
| `1001`, `1012`, `4000`, `4002`, abnormal closure | reconnect with backoff, then catch up |
| **any close code not listed here** — `1000`, `1009`, `1011`, or one a later server adds | **reconnect with backoff** |

***Preceded by*** means the **last frame before the close, carrying no `client_id`**. An earlier `error{rate_limited}` naming some `send` does not make a later bare `1008` read as transient.

**`error{internal}` is never terminal, whatever its `retryable` says.** An unclassified server failure reaches the client as `internal`, and that flag is deliberately biased toward `true`, so it cannot decide terminal — and a clearly permanent server-side fault must not stop every client at once and keep them stopped after the fix.

### The bare `1008` producers — five, since CANT-122

Six sites closed `1008` with **no** preceding `error` frame, and five still do. `refuse()` writes a frame first, so its closes are not in this set.

| site | cause | |
|---|---|---|
| `socket.go:516` | hello timeout | **transient — closes `4002` (`statusHelloTimeout`), no longer `1008`**, since CANT-122 |
| `socket.go:535` | first frame was not a hello | client bug |
| `socket.go:593` | a second hello | client bug |
| `socket.go:765` | a malformed frame | client bug |
| `socket.go:435` | `resume_from_log_seq` negative | client bug, decoder-unreachable |
| `hub.go:953` | `up_to_seq` below 1 | client bug, decoder-unreachable |

The last two are unreachable behind the generated decoder (`Seq` carries `minimum: 1`).

**A bare `1008` is a client bug only against a server that carries `4002`.** One that predates CANT-122 still closes the hello timeout with a bare `1008`, so the deployed server carries it before any client applying this table points at it — `soakrig`'s `internal/client` included. A rollback past it reintroduces the hazard, and a relaunch recovers, because a protocol terminal keeps the credential (§6).

**`unauthorized` on this door means a device mismatch only** — a bad or expired credential never reaches a socket, because it is refused with HTTP 401 at the upgrade.

## 5 · Terminal, by HTTP

| observation | client does |
|---|---|
| 401 on `/sync` | single-flight refresh, then retry **once** |
| 401 on `/refresh` — **Catenary's own** `{"code":"unauthorized"}`, **and** the stored credential is still the one presented | **terminal.** The credential is gone. |
| 401 on `/refresh` that did not come from Catenary — a proxy, an expired Access session in front | **not terminal** |
| 401 on `/refresh` for a credential another context already rotated | **not terminal.** Re-read the persisted credential before concluding anything. |
| `/refresh` answers `503`, another 5xx, or a network error | an unknown outcome — §3 |

The deployed path runs through `cf-access-jwt` and `cf-access-guard`, so a 401 from a hop in front says nothing about the Catenary credential. **`refresh_expires_at` having passed is not by itself terminal** — that would be a terminal decision taken on the device clock with no round trip.

## 6 · What a terminal state is, and what ends it

| terminal | ends when | the person is told |
|---|---|---|
| **credential** — §5 row 2 | re-checked **once on relaunch**; otherwise only on re-enrollment | re-enroll this device |
| **protocol** — `4001`, or a `1008` that §4 makes permanent | **on relaunch** (an app update is a relaunch) | update the app, or report a bug |

**Neither ends on a timer or on a network change.** The client's status names which terminal it is in, distinguishably from an exhausted backoff.

**Terminal never deletes the stored credential or the local store.** That makes a false terminal recoverable by relaunch, and costs a true one nothing, because the server refuses that credential forever anyway.

## 7 · The outbox

**A `send` written in a session that ended with a *bare* `1008`, and never acked, goes to `failed` and is not requeued.** A bare `1008` is the only close meaning the server could not parse what the client wrote; replaying it walks into the same close, and the person's obvious remedy — clear data, reinstall — throws away the credential.

**A send from a session closed with a frame-preceded refusal is explicitly not covered.** Those sends were never processed and are not at fault: after an app update fixes a `wire_version_unsupported`, they should go out.

Retrying a send across any of these events is safe on the server's side: `client_id` dedup means a repeat cannot store a second message.

## Stated here, built elsewhere

- **The TypeScript transport and its state machine** — CANT-35.
- **The Dart transport and SQLite local store** — CANT-42.
- **The outbox state machine**, including §7's obligation — CANT-36 and CANT-42.
- **Cross-client convergence**, the only mechanical check that the two clients agree — CANT-46.
- **A user-revocation publisher** — CANT-33. The hub's `UserID` branch exists and is tested; nothing writes `users.deactivated_at` yet. §4's `4001` row is written for both subjects now so it is not retrofitted.
- **`ReuseGraceWindow`** stays as CANT-29 set it, and this plan adds no second window beside it.
