# CLAUDE.md — Catenary

A self-hosted chat service for a small trusted group. Text, images, voice notes and server-side transcription of them, across web, Linux desktop and Android. Single static Go binary, sibling to the other construct-server Go services.

Tracked in Switchyard under the **CANT** project — 10 epics (`CANT-1`…`CANT-10`), 60 tickets (`CANT-11`…`CANT-70`). It graduated there from `IDEA-23` with all six P0 gates cleared on measured evidence. The key is `CANT` — railway superelevation, the banking on a bend, on theme with Switchyard and Interlock — and not `CTNR`, which collides in shape and in kind with Centrifuge's `CTFG`.

## Layout

- `cmd/catenary/` — entrypoint + subcommands (`serve`, `migrate`, `version`). Composition root: `setup()` wires store + services + router.
- `schema/` — **the wire contract.** One schema, the generator over it, and 41 conformance vectors that TypeScript, Dart and Go all have to agree on, with a staleness guard that fails the build on a hand edit. Everything else here is downstream of this directory, and nothing in it is edited by hand.
- `internal/config/` — env-only config, `CATENARY_`-prefixed.
- `internal/store/` — pgx pool, embedded migrator, repo queries, and the domain types themselves. Types sit beside the queries that return them rather than in a separate `internal/model/`.
- `internal/api/` — the HTTP and WebSocket surface.
- `migrations/` — `NNNN_name.up.sql` / `.down.sql`, embedded, auto-applied on boot.
- `web/` — Vue 3 + TypeScript client, served by the Go binary. `npm run smoke` SSRs the app and asserts the design canvas's landmarks are actually on screen.
- `dart/` — the generated Dart wire package and its conformance runner. The Flutter client lands here in E5.
- `server/` — the generated Go wire package and the Go conformance runner.
- `deploy/` — compose fragment and Traefik labels.
- `spike/` — **the P0 evidence, and read-only history.** R1's tunnel rig, R2's push harness, R3's whisper benchmarks, R6's Purser stub, each with its own `FINDINGS.md`. Read them before re-deriving anything they already answer; `SPIKE-RESULTS.md` is the one-page index.

## Conventions (match the construct-server house style)

- Go 1.26, `pgx/v5`, `google/uuid`. No ORM. No external migration tool — an in-process migrator applies embedded SQL.
- Config is env-only, `CATENARY_`-prefixed, with a `DATABASE_URL` fallback. No config files.
- Logs: structured, to stdout, in a shape Dozzle and Datadog can read. Health: `GET /healthz` and `GET /readyz`.
- Release-please + GHCR image `ghcr.io/einlanzerous/catenary`. Conventional commits.
- Its own database and its own role on the shared Postgres 16. Credentials live in Signet, never in a compose file.
- Design tokens: ten names shared with the Flutter client. **Names are the contract, values are per-theme.** One copper accent carries unread, active, playing and read, and nothing else competes for it.

## Invariants — don't break these

### 1 · Both ordinals are assigned inside the insert's own transaction.

Every message carries two ordinals. `seq` is per conversation and orders a thread — it drives `first_unread_seq` and the "N NEW" rule. `log_seq` is account-global and sparse, and is the only thing `/sync?after=` takes. Two rather than one, because a scalar cursor over per-conversation sequences is not well-defined, and a per-conversation cursor map cannot bootstrap a client that has never heard of a conversation.

**A `bigserial` here loses acked messages permanently and never fails a single-threaded test.** A sequence hands out its number outside the transaction, so two inserters can commit out of order: a client that has seen `log_seq` 100 will never ask for 99, and the message holding 99 is gone from every sync that follows. It stays invisible until it is somebody's message that never arrived.

So both ordinals come from a single-row counter inside the same transaction as the insert, and **ascending `log_seq` must imply ascending commit order.** The property test in `CANT-19` is the enforcement mechanism, and it is written so that it fails against a `bigserial`.

### 2 · One schema, three generated languages. The types are never hand-written twice.

D3 splits the client in two, which means the sync protocol gets implemented twice in two languages. Divergence between them shows up as ghost messages, duplicate sends, and unread counts that disagree across devices — the exact failure class this project exists to avoid.

The wire schema is the source of truth and the only place a message type is defined. TypeScript, Dart and Go are generated from it, 41 golden vectors are the contract between them, and a staleness guard fails the build the moment a generated file is hand-edited. Retrofitting this after two clients have drifted is far worse than paying for it up front, which is why R4 was a gate rather than a P1 task.

The one deliberate exception is the **frame union**, where the Dart generator's handling is bad enough that a bespoke generator may earn its keep. `CANT-12` settles exactly where that line sits; until it does, assume the house OpenAPI pipeline owns the REST surface.

### 3 · The client never claims something the server cannot keep.

The thread header reads `7 MEMBERS · TLS`, not `E2E`. D1 declines end-to-end encryption and names its mitigation as "honesty with users about what the server can see" — a badge asserting the opposite is the one claim this surface must not make.

The wire schema follows the same rule from the other direction: `sending`, `queued` and `failed` are **not expressible on the wire**, because they describe a message's relationship to its own outbox and the server has no opinion on them. That is also why the two client tickets that own them have no oracle.

Derive rather than store wherever the two could disagree. There is no stored unread count — the badge and the "N NEW" rule both come from `first_unread_seq`, so they cannot drift apart, and neither counts your own messages, because you cannot have an unread message you sent. Transcript word counts are derived from the text on screen, so "EXPAND · 96 W" cannot lie. Waveform peaks are the inverse case, and stored deliberately: they are computed **server-side**, because the seeded generator overflows 2^53 and produces different bars in JavaScript than in Dart.

### 4 · Transcription is a client of the estate ASR service. It is never a second queue.

One whisper.cpp/Vulkan runner on the R9700 serves the whole estate, behind an HTTP job contract with a single-flight GPU lease. Chronicle is client one; Catenary is client two. **Two services queueing work to one GPU with two independent schedulers is a resource-contention bug, and the second one written is the one that discovers it** — in production, as timeouts in both. The R9700 also hosts Ollama, so there is already a third consumer.

The contract is `asr/openapi.yaml` in the chronicle repository and the guide is `asr/CLIENT.md`. Generate a client rather than hand-writing one, and pin the spec by an `asr-v*` tag, because the service versions independently of Chronicle. Submit audio as recorded — the service decodes, because doing that once there beats doing it in every client. Plan against the **60-second** model-switch bound rather than the reassuring `small.en` throughput number, which only holds while both clients want the resident model.

It is not storage. Submitted audio is deleted the moment a job reaches a terminal state, everything in it is regenerable from audio we still hold, and we keep our own recordings.

## Working agreement

Reviewing sixty diffs does not scale, and long autonomous runs accumulate unreviewed *decisions* rather than unreviewed lines — by the time a PR appears, three of them are load-bearing. So review mode is chosen per ticket and carried on the ticket's own `review_mode` field in Switchyard, and decisions are written down at ticket boundaries as they happen, never batched to the end of an epic.

| mode | `review_mode` | what the reviewer sees | tickets |
|---|---|---|---|
| **A · evidence** | `evidence` | the `Done when` claim and green CI — not the diff | 40 |
| **B · decision first** | `decision` | a written decision *before* any code; the PR is then mechanical | 15 |
| **C · full diff** | `full` | every line | the five below |

**Mode B is enforced rather than encouraged.** Switchyard refuses to move a `decision` ticket into In Progress until its plan is approved, returning a 422 that names the reason. Open the plan and get it approved, then build. Do not ask for the mode to be lowered instead.

**Mode C is exactly five tickets, and the list does not grow by habit:** `CANT-14` (both ordinals in one transaction), `CANT-22` (WebSocket upgrade and auth handshake), `CANT-29` (refresh rotation with reuse detection), `CANT-63` (edits and deletes), `CANT-67` (retention sweep). The rule that generates that list: *anything that can destroy authored messages, or hand an agent write access to them.*

Three tickets look like they belong on it and deliberately do not. `CANT-13`'s initial migration runs against an empty database — there is nothing there yet to destroy, and it is Mode B because its *shape* is the risk. `CANT-68`'s restore drill is reviewed as a **result** — a restore that ran, worked, and was timed — rather than as a diff. `CANT-33`'s Purser connector deletes accounts rather than messages, and R6 already wrote seven tests against the real interface.

**The tier is not the mode.** Model tier tracks the strength of the oracle — 19 Opus, 37 Sonnet, 4 Haiku, chosen by asking whether a machine can tell you the work is wrong. Mode tracks what a human reads. They correlate, and they are not the same axis: `CANT-67` is a Sonnet ticket read line by line, because it deletes messages on a timer.

### Mechanics

- One worktree per epic. Branch `CANT-NN-description` per ticket. **The ticket key comes first**; its case is not load-bearing, so `cant-67-...` is equally fine. What matters is that the key leads, so per-ticket spend attributes correctly and a branch says what it is at a glance. Release-please reads the commit message, so the `feat:` / `fix:` prefix belongs there and not in the branch name.
- `./verify.sh` green before anything is handed over: every check that does not need hardware, in one command.
- Every ticket closes with a Switchyard transition **and** a comment carrying its evidence. **The board is the status surface** — nobody should have to scroll a session transcript to learn where things stand. That is also why this file has no status section: it is the shared prefix every agent inherits, and it stays byte-identical so caching hits.
- Fan-out is only affordable while that prefix stays identical. Never interpolate anything per-agent above it.

### Stop and ask when

- a `Done when` cannot be met as written,
- a decision surfaces that the ticket does not settle,
- or the change wants to touch something outside its own epic.

Everything else runs to completion and is reviewed afterwards.

## Testing

`go test ./...`, `npm run smoke` in `web/`, the Dart conformance runner in `dart/`, and `./verify.sh` for the full non-hardware suite. CI builds the binary, runs all three conformance runners against the same 41 vectors, and fails when a generated file and its schema disagree — a generated artefact with no guard is a generated artefact someone hand-edits.
