# CANT-28 — the token model: four credential shapes, one seam, one encoding

> **The Switchyard plan is the decision of record.** It is versioned, its criteria carry verdicts and its rulings carry picks, and it gates the ticket: **Switchyard `CANT-28`, plan rev 3, approved** (Switchyard is estate-internal, so there is no link that resolves from a clone of this public repository). This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale.

One migration, `0007_tokens`. One store file, `internal/store/tokens.go`. One new route, `POST /enroll`. `GET /sync` is registered in a real process for the first time.

## Rulings, as settled

| | settled |
|---|---|
| **0 · what an access token is** | **Opaque random, stored hashed, looked up per request.** No signing key, nothing for Signet to hold, and revocation takes effect on the next request rather than at the next expiry. Costs an `access_tokens` table and one indexed point-read per authenticated call. |
| **1 · where the credential rides on the upgrade** | **`Sec-WebSocket-Protocol`**, the one request header a browser can set on a WebSocket. Two values offered, one echoed — see below. |
| **2 · socket authorization** | **Once at accept.** The session outlives the access token that opened it; a revocation severs it through the fanout. An access token expiring under a live socket is not an event. |
| **3 · lifetimes** | **Access 15 minutes · refresh 60 days · enrollment 7 days.** |
| **4 · bot token scope** | **Membership alone**, enforced by `not_a_member`, which already exists. |
| **5 · where the `/refresh` handler lands** | **A sub-task of CANT-29, filed `review_mode: full`** — `CANT-97`. Its payload types are in the wire schema from this ticket; only the handler moved. |
| **6 · where the credential payload types live** | **The wire schema**, as `$defs` with conformance vectors. The count moved 41 → 48. |
| **7 · how a revocation crosses instances** | **Its own channel, its own payload type** — `catenary_device_revoked`, `RevocationPayload`. `NotifyPayload` is untouched and CANT-92's decision stays CANT-92's. |
| **8 · CANT-31's `Done when`** | **Amended**, to REST refresh that does not disturb a live socket plus a clean reconnect after severance. |

## The four shapes

| shape | table | rotates | expires | minted by |
|---|---|---|---|---|
| **enrollment** | `enrollment_tokens` | no — re-issued, superseding | 7 days | `IssueEnrollmentToken`, called by Purser's Provision |
| **refresh** | `refresh_tokens` | yes, single-use | 60 days | `RedeemEnrollment` here; rotated by `CANT-97` |
| **access** | `access_tokens` | n/a | 15 minutes | `RedeemEnrollment` here; re-issued by `CANT-97` |
| **bot** | `access_tokens`, `device_id IS NULL` | no | never | `IssueBotToken`, from CANT-69 or a CLI subcommand — never Purser |

A bot has no device. `access_tokens` CHECKs `(device_id IS NULL) = (expires_at IS NULL)`, so a person's fifteen minutes cannot become forever by one bad INSERT; the other half of the shape — that a device-less token's user is `kind = 'bot'` — is a store invariant, because a CHECK reads one table.

A **bot token grants append-as-self only**: no edit, no delete, including of its own messages. That is what keeps CANT-73 off the Mode C list.

## One encoding, for all four

**32 bytes from `crypto/rand`, rendered base64url without padding.** 43 characters. One helper, `store.MintToken`, and one format in every log, config file and Signet entry.

`Sec-WebSocket-Protocol` carries RFC 6455 subprotocol names, each an RFC 7230 `token`. **`/` and `=` are not legal in one; `+` is** — so the rule is the base64url alphabet rather than a blocklist of base64's specials, which would be both wrong about `+` and silently permissive on a token containing only it. The wire schema's `Token` pattern is that alphabet, so all three generated decoders enforce it and neither client can get it wrong quietly.

**Two values offered, one echoed.** The client offers `catenary.v1` and `catenary.token.<token>`; the server verifies the second and echoes back **only** `catenary.v1`. Echoing the credential would put it in a `Sec-WebSocket-Protocol` response header on the 101, which is a header proxies log — reinstating the objection that ruled out a query-string token. CANT-22 implements this and both clients are measured against it.

**Every token is stored as a SHA-256 hash and never in the clear.** Not a password KDF: there is no dictionary over 32 random bytes, and ruling 0 makes verification an indexed point-read that a per-row salt would forbid. The entropy is the defence; the hash is so a database copy is not a key ring.

## One seam

`store.Authenticate` is the only thing in this service that resolves a credential, and it refuses on four conditions: the token does not resolve, is expired, or is revoked; the **device** is revoked; the **account** is deactivated; or a bot attempted a device-only operation. `CallerID` on the router is an adapter over it and CANT-22's upgrade calls the same function. A guard test fails the build if anything outside `internal/store` names a credential table.

**A deactivated user can neither redeem an enrollment token, nor authenticate, nor — from `CANT-97` — exchange a refresh token.** R6 chose disable-then-revoke for Purser's offboard over the reverse ordering on exactly that sentence, so its correctness argument depends on this being true here rather than assumed. Before this ticket no query in this service read `users.deactivated_at`.

REST carries the access token as `Authorization: Bearer`. Never a query parameter: Traefik and Cloudflare log request lines.

## One refusal

`POST /enroll` is the only unauthenticated credential-minting route this service has, and it has **no rate limiter** — declined with reasons in the plan, not overlooked. So **every credential failure returns the same 401 with the same body**, byte for byte: unknown token, expired, already redeemed, superseded, deactivated account. It is the same body `GET /sync` writes. The log distinguishes all five, with the token id where one resolves and never the token itself; visibility is what stands in for the limiter, and it has to be one-directional.

A malformed request — unparseable JSON, a token that is not shaped like one, a missing device name — is a 400 and is not part of that rule. None of them says anything about whether a credential exists.

## What CANT-97 and CANT-29 inherit

`refresh_tokens` carries `family_id` and `replaced_by` from this migration, written by nothing in this ticket. **The first token of a family is its own family** — `family_id = id` — so invalidation is one predicate over one column rather than a walk back up a chain.

The rotation write is specified here and implemented there, as a **conditional update rather than a lock**, in one transaction:

1. insert the new `refresh_tokens` row;
2. `UPDATE refresh_tokens SET replaced_by = $new WHERE id = $presented AND replaced_by IS NULL AND revoked_at IS NULL RETURNING id`;
3. zero rows means somebody else rotated first — roll back, and the row from step 1 goes with it.

Under READ COMMITTED the losing `UPDATE` blocks on the winner's row lock, re-evaluates its `WHERE` against the committed version, and matches nothing. That is the whole of the atomicity, and it is what makes two in-flight requests from a waking phone produce one new pair and one refusal rather than a forked family. **A replay is then exactly a presentation whose conditional update returns zero rows against a row that already has `replaced_by` set** — so CANT-29's detection is a query over rows this ticket's shape guarantees, not a restructuring.

No index on `family_id`: the column has to exist now because adding it later rewrites a table of live credentials, but the index belongs to the ticket whose query needs it.

## Locks

Redemption takes `FOR UPDATE` on the one `enrollment_tokens` row and **no lock on `users`** — `deactivated_at` is read unlocked, and the `devices` insert takes `KEY SHARE` through its FK, which `internal/store/messages.go:66` already argues is safe against a `FOR NO KEY UPDATE` deactivation. `log_counter` is never drawn.

**The race that leaves open is closed at authentication, not at enrollment.** A deactivation committing between the read and the insert lets a device row be created for a now-disabled account; every request from it is then refused. A stray row and no access — R6's explicitly-accepted half-done state, from the other direction.

`RevokeDevice` is a non-key `UPDATE`, so `FOR NO KEY UPDATE`, which the same note anticipated. It is idempotent by its `revoked_at IS NULL` guard, because R6 requires Deprovision to be safe to retry.

## Severance

`RevokeDevice` writes `devices.revoked_at` and publishes on `catenary_device_revoked` **inside the same transaction** — CANT-18's ruling 2 applied to a second event, so a notification cannot exist without its cause or the reverse. The write lands here rather than in CANT-30 because that property cannot be handed to a caller as a convention.

`RevocationPayload` carries a subject that is a **device or a user**. The user half has no publisher yet: the write that sets `users.deactivated_at` is CANT-33's connector surface over an admin API that does not exist. **Until it does, a deactivated account's live socket survives until it drops** — CANT-30's `Done when` covers devices, and R6's sentence is about request paths.

Severing the live socket is CANT-30's. So is the gap case: Postgres queues nothing for a disconnected listener and a revocation has no cursor, so on `OnGap` an instance must re-check its own live sessions against the database.

## The deploy bar takes three tickets

`deploy/README.md` said no public router until CANT-22 and CANT-28. It is **CANT-22, CANT-28 and CANT-29** — rotation without reuse detection leaves an attacker who rotated first holding a live family while the real device's refusal looks like a bug, and no ruling here could close that. Corrected in that file.
