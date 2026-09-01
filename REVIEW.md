# Review instructions

What a review of **this** repository is for. The procedure lives in
construct-server; this file is the judgement.

Catenary is an append-only message log with a sync protocol. The thing it must
never do is lose or duplicate an authored message, and the second thing is claim
to users a property it does not have. Weigh findings against those two.

## Why this review carries more weight here than in a repo with human diff review

`CLAUDE.md`'s working agreement puts **40 of 60 tickets in Mode A**, where the
reviewer reads the `Done when` claim and green CI — *not the diff*. On those
tickets **you are the only read the code gets.**

That cuts both ways. It means a missed Important finding ships unseen; it also
means a review that lists twelve nits and no substance has consumed the only
attention the change will receive. Spend the budget where the invariants are.

15 further tickets are Mode B, where a written decision was approved *before* any
code and the PR is supposed to be mechanical. There the question is not "is this
good code" but "**is this the approved decision**". See ticket fidelity.

## What CI already proves — and where it does not

`./verify.sh` runs in CI, whole. Do not spend findings re-deriving what it
already checks:

- the generated TypeScript, Dart, Go and `openapi.yaml` match the schema, and
  the staleness guard is proved to fail **once per file**;
- all three conformance runners pass the same 41 vectors;
- `gofmt`, `go vet`, `go test ./...` for both modules, with a real Postgres 16;
- no document describes `log_seq` as "account-global", proved against a planted
  line every run.

Where it does not reach, and where you therefore should:

- **Three steps skip rather than fail** when what they need is absent — R1's
  tunnel, R2/R5's Android device, and R6, which needs the Purser checkout next
  door. A skip is not a pass. If a diff changes something those cover, say so.
- **The wire-field mapping guard checks types, not truth.** It fails on an
  unmapped field or a column whose type contradicts the wire. Nothing checks
  whether a `derived` entry's stated derivation is *correct*.
- **Conformance vectors are hand-written.** A weakened vector is how a suite
  goes quietly green; `schema/vectors/vectors.json` is deliberately not excluded
  from review.
- **Green CI on a Mode C ticket means nothing about the thing that makes it Mode
  C.** No ordinary test catches silent, permanent message loss.

## Ticket fidelity — check this first

Find the ticket key in the branch name or PR title (`CANT-NN`). Then:

**Read the ticket's `review_mode`.** It is stated on the ticket, never inferred.

- **`evidence` (Mode A)** — verify the `Done when` is actually met, clause by
  clause, and say which clauses you checked. A `Done when` quietly narrowed in
  the PR body is an Important finding.
- **`decision` (Mode B)** — **the approved plan's criteria are the acceptance
  contract, not the ticket's `Done when`.** The ticket is the summary; the plan
  is the decision of record. A PR that satisfies the summary and departs from
  the plan is an Important finding even if the code is good — and a deviation
  the author *flagged* is not a finding, it is the process working.
- **`full` (Mode C)** — five tickets, and the list does not grow: `CANT-14`
  (both ordinals in one transaction), `CANT-22` (WebSocket upgrade and auth
  handshake), `CANT-29` (refresh rotation with reuse detection), `CANT-63`
  (edits and deletes), `CANT-67` (retention sweep). A human reads every line, so
  do not duplicate that. Report only what a line-by-line read plausibly misses:
  concurrency, ordering, and what happens on the failure path.
- **unset** — ask rather than assume. An unset mode is not permission.

**Scope.** The working agreement says a change that wants to touch something
outside its own epic is a stop-and-ask. A PR that quietly does it anyway is a
finding — small ones especially, because they are how the review modes erode.

## Severity

- **🔴 Important** — can lose, duplicate or resurrect an authored message;
  breaks either ordinal's contract; makes the client claim something the server
  cannot keep; leaks or logs credential material; widens who can reach an
  endpoint; stops the server booting; hand-writes a type the schema owns; or
  does not do what the ticket or the approved plan asked.
- **🟡 Nit** — conventions, clarity, a comment that will mislead. Never blocking.
- **🟣 Pre-existing** — real, not introduced here. At most two per review.

Cap nits at five; beyond that say "plus N similar" in the summary. A review that
buries one Important finding under twelve nits has failed at its job.

## Always check

### 1. Both ordinals, and the `bigserial` trap

`seq` is per conversation and **dense** — a client that can see 4 and 6 assumes 5
exists and it is missing it. `log_seq` is **server-global** and **sparse**, one
counter for the whole deployment, and is the only thing `/sync?after=` takes.

Flag as Important:

- a **`bigserial`, `SERIAL`, `IDENTITY` or `nextval()`** anywhere near either
  ordinal. A sequence hands out its number outside the transaction, so two
  inserters can commit out of order and the message holding the lower value is
  gone from every later sync. **It never fails a single-threaded test.**
- an ordinal drawn **outside** the inserting transaction, or the insert and the
  counter bump in different transactions.
- anything that can **commit** a transaction which drew an ordinal and inserted
  no row — `INSERT … ON CONFLICT DO NOTHING/DO UPDATE` after the draw is the
  specific idiom. It burns a `seq` permanently, and the wire tells every client
  a `seq` gap means a lost message. The idempotency check must come **before**
  either draw.
- a second `log_counter` row, or anything making the counter per-account. That
  would be dense, and it inverts the whole division of labour, silently.
- prose calling `log_seq` "account-global". `verify.sh` greps for it; `spike/`
  is exempt because it is dated evidence.

### 2. One schema, three generated languages

The wire schema is the only place a message type is defined. TypeScript, Dart
and Go are generated from it.

- A **hand-written wire type**, or a hand edit to a generated file, is
  Important. The guard catches the edit; it cannot catch a parallel type
  declared somewhere else.
- A **new or changed wire field** must land in `schema/mapping/wire-fields.json`
  as `column`, `derived` or `client-local`. The guard enforces that it is
  mapped; **you** are what checks the note is true.
- A **weakened conformance vector** is Important. So is a vector added for a
  behaviour that is not in the schema.

### 3. Derived rather than stored, wherever the two could disagree

There is no stored unread count: the badge and the "N NEW" rule both come from
`first_unread_seq`, so they cannot drift. `head_seq` is served from
`conversations.last_seq` and is not a column. Transcript word counts come from
the text on screen.

A new **stored** column that duplicates something already derivable is
Important — say what the two sources are and how they come apart.

The deliberate inverse: **waveform peaks are stored**, computed server-side,
because the seeded generator overflows 2^53 and yields different bars in
JavaScript than in Dart. A change moving that computation client-side is
Important.

### 4. The client never claims what the server cannot keep

D1 declines end-to-end encryption and names its mitigation as honesty about what
the server can see. The thread header reads `7 MEMBERS · TLS`, never `E2E`.

- A badge, string or doc asserting encryption, privacy or delivery the server
  does not provide is Important.
- `sending`, `queued` and `failed` are **not expressible on the wire** — they
  describe a message's relationship to its own outbox. A PR putting them in the
  schema is Important.

### 5. Transcription is a client of the estate ASR service

One whisper runner serves the whole estate behind a single-flight GPU lease.
**A second queue, a second scheduler, or a direct whisper invocation is
Important** — two services scheduling one GPU is a contention bug that shows up
in production as timeouts in both, and the second client written is the one that
discovers it. The client is generated from `asr/openapi.yaml` in the chronicle
repository, pinned by an `asr-v*` tag.

### 6. Credentials and grants

Credentials live in Signet, never in a compose file, and this repository is
**public**. Flag any credential-shaped literal that is not obviously a throwaway
for an ephemeral test container.

`deploy/provision.sql` grants are justified by what they **do**, not by what the
comment says — CHRN-78 exists because a `REVOKE` was explained by a false claim,
and CHRN-79 because the claim had been copied into a second document. If a grant
and its comment disagree, the comment is the bug and it is Important.

### 7. Migrations

Every migration after the first is a delta against a table with history in it.

- An `.up.sql` with no `.down.sql` — the migrator refuses one, so this is really
  a check that the refusal still holds.
- A destructive statement (`DROP COLUMN`, `DROP TABLE`, a `DELETE` without a
  bounded predicate) against `messages` or `attachments`.
- An `ON DELETE` that lets a user or device deletion reach authored messages.
  `messages.author_id` is `RESTRICT`, and that single fact is what keeps
  `CANT-33` off the Mode C list.
- `retention_days`: `NULL` means inherit-global-infinite, **not zero days**.
  Code reading it as 0 deletes everything.

### 8. Logs

Structured, to stdout, one JSON object per line. Successful probe requests log
at debug; anything answering 4xx or 5xx keeps its level — the `/readyz` 503 is
the most important line this service emits. A log line carrying message text,
a token, or a DSN is Important.

## Verification bar

Report a finding only when you can point at the line that causes it and name the
concrete failure — the input, state, or sequence producing the wrong outcome.
"This could be risky" is not a finding.

Behaviour inferred from a name is not evidence. If you find yourself writing
"this may not handle…", read the implementation or drop it.

**Verify claims made in comments and PR bodies.** This repository's comments are
unusually load-bearing and carry reasoning not recoverable from the code, which
is exactly why a false one is expensive. The same applies to a PR body asserting
a criterion is met.

Where a probe is cheap, run it. The ordinal, grant and `ON DELETE` questions
above are decidable by reading `migrations/` rather than by reasoning about what
a constraint probably does.

## Re-reviews

Round three should be shorter than round one. After the first review of a PR,
report **new Important findings only** — no new nits, no restating open
findings, no re-raising something the author explicitly declined. Note in one
line what got fixed, then move on.

Check that a fix was applied **at the layer that owns the invariant**, not at the
call site that happened to be reported.

## Summary shape

Open with a one-line tally — `2 important, 1 nit` — or **No blocking issues**.
Then ticket fidelity in a sentence, naming the review mode. Then findings, most
severe first, each with the file, the concrete failure, and what would fix it.

Close with what you checked and could not fault. **On a Mode A ticket you are
the only diff read**, so a review listing only problems does not tell the human
what was examined.

If the diff is clean, say so in one line and stop. Do not pad.
