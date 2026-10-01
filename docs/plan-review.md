# Reviewing a plan

What the adversarial plan pass verifies a **CANT** plan against. `REVIEW.md` is
the judgement for a diff; this file is the judgement for a plan, before any diff
exists. The procedure — the workflow, the verdict file, the check row — lives in
Switchyard (`.github/workflows/plan-review.yml` and `docs/plan-as-pr.md` there);
this file is what a Catenary plan has to get right.

Every check below is performable from **this checkout plus the plan JSON**, and
nothing else. Where a check needs a command, the command is given.

## Who reads this and what they hold

The pass is CI for a plan revision. It runs on a read-only checkout of `main`
with Bash, Read, Grep and Glob, and three files on disk:

- `plan.json` — the revision: `sections[]` (each with an `anchor`), `criteria[]`
  (each with a `method`), `rulings[]` (0-based `position`, `options[]`,
  `recommended_position`, `chosen_option_id`, `required`, `liveness`,
  `gated_by_position`, `gated_on_option`), `work_breakdown[]` (each row with a
  `review_mode` and `depends_on`), and the prior `reviews[]` and `threads[]`.
- `ticket.json` — the ticket's `key`, `title`, `type` and `review_mode`.
- `plan-context.json` — the ticket's `review_mode` and whether the revision
  carries a `live_required_ruling`.

It has **no Switchyard tools and no Switchyard token**, on purpose: the check
row's authorship is treated as a signature, and a pass holding the writer's
token would be the vouch approving itself. Do not compose a Switchyard request;
do not read the operator's credential files. Everything you need is on disk.

It writes **one verdict file** and nothing else:

```json
{ "blocking":  [ { "section_anchor": "approach", "quoted_text": "…", "body_md": "…" } ],
  "advisory":  [ … ],
  "summary_md": "what you looked at, what you found" }
```

`blocking` becomes one `changes_requested` review through the ordinary door and
re-queues the planner. `advisory` becomes anchored threads on a **passing**
check. `section_anchor` must name a section in *this* revision — one that does
not is refused and the finding is dropped, so anchor a criterion finding on the
section that holds it and quote the criterion's leading words. An empty
`blocking` with a written `summary_md` is a clean pass, and it must say what was
checked, not that checking happened.

## Checks

Each is stated with what makes it blocking and how to perform it here. Checks
2–6 are this repository's invariants restated for a plan; a plan that violates
one is bounced, not advised.

### 1. Every cited path, symbol, line and count exists in this checkout

A plan leans on the tree it describes. Verify each citation before letting the
plan lean on it: `Glob` the path, `Grep` the symbol, `Read` the cited line.

- **A wrong citation is blocking when the plan's argument rests on it**;
  advisory when it is a typo beside a correct argument. CANT-31 rev 4 cited
  `senderror.go:182-189` for how an unclassified failure is flagged; that is
  `Wire()`'s nil-receiver branch, which the real path never takes, and the
  plan's terminal rule was built on the misreading. CANT-141 rev 2 named
  `TestWireMapMatchesTheDatabase` as the guard that goes red without a mapping
  entry; the DB-free guard is `TestEveryWireFieldIsMapped`
  (`internal/store/wiremap_test.go`), and a reviewer running the criterion's
  test would have seen no failure.
- **A premise the tree already contradicts is blocking.** CANT-139 was filed
  saying `FindOrCreateDirect` opens a room "with no look at
  `users.deactivated_at`"; `internal/store/metadata.go` had refused a
  deactivated target for eight days, with `TestFindOrCreateDirectRefusesADeactivatedTarget`
  behind it. Read the function before accepting the problem statement.
- **A claim about a neighbouring repository is `unverified`, never `false`.**
  Purser's connector interface, the estate ASR service's `asr/openapi.yaml`
  and its `asr-v*` tags live in other checkouts. Say "unverified from this
  checkout" in the finding and move on; do not bounce on it.
- **Counts are looked up, not copied.** The vector count is
  `jq '.cases | length' schema/vectors/vectors.json`; `CLAUDE.md` states it in
  three places and `README.md`'s R4 row records the `41` of the day the gate
  cleared. A plan that quotes a count must quote today's, and a plan that adds
  a case must say the count moves and where (CANT-140 rev 1 carried both a
  stale `85` and the correct `89` in different sections).
- **A state the plan describes as open may have closed since it was written.**
  CANT-127 rev 1 described CANT-118 as "stopped on a question" that a person
  had settled after the plan was submitted, and rev 2 still said "PR #77 is
  open" after it merged. Where a plan cites a PR, a constant or a sibling
  ticket's state, read the tree for the artefact: does the constant, file or
  test that change would have landed exist on `main` now? Not `git log` — the
  pass runs on a one-commit checkout, where a path's history is the tip or
  nothing, and an empty history would read as "nothing changed".

### 2. The two ordinals — blocking

`seq` is per conversation and **dense**; `log_seq` is server-global and
**sparse**, and is the only thing `/sync?after=` takes. Both are drawn from
single-row counters inside the inserting transaction (`log_counter` with
`CHECK (id = 1)`, `migrations/0003_messages.up.sql`; `conversations.last_seq`
under the row lock). The enforcement is
`internal/store/logorder_test.go` — `TestForcedScheduleLosesAMessageWhenLogSeqComesFromASequence`
is the failure written down, `TestOrdinalsAreNotDrawnFromASequence` is the
guard.

Bounce a plan whose proposed SQL or prose contains any of:

- `bigserial`, `SERIAL`, `IDENTITY` or `nextval()` for either ordinal, or for
  anything `/sync` orders by. It never fails a single-threaded test and loses
  acked messages permanently.
- an ordinal drawn outside the inserting transaction, or the draw and the
  insert in different transactions.
- `INSERT … ON CONFLICT DO NOTHING/DO UPDATE` **after** the draw — a burned
  `seq` reads as a lost message to every client. `docs/decisions/cant-13-schema-and-migrations.md`
  ruling 2: idempotency is checked before either draw.
- a second `log_counter` row, or a per-account counter. That is dense, and it
  inverts the division of labour silently.
- the known-wrong phrase `account-global` for `log_seq`, which the wire schema
  records as a corrected earlier draft. `verify.sh` greps the tree for it; a
  plan is not in the tree, so you are the grep.

A plan that moves a marker (`conversations.metadata_log_seq` and its kin) must
draw it through `internal/store/metadata.go` — `metadata_guard_test.go` bans
the writes that must move a marker anywhere else, and a plan proposing such a
write outside that file will fail the build it describes.

### 3. One wire schema, three generated languages — blocking

`schema/catenary.wire.v1.schema.json` is the only place a wire type is defined;
`schema/codegen/generate.mjs` emits TypeScript, Dart, Go and `openapi.yaml`,
and `gen:check` fails the build on a hand edit. The decoders enforce the
schema's constraints — `checkSeq`, `checkLogSeq`, `checkToken` and their
neighbours in `internal/wire/generated.go`.

- **A hand-written wire type, or a parallel declaration of one, is blocking.**
- **A changed frame or payload names which vectors move**, by name from
  `schema/vectors/vectors.json` (`jq -r '.cases[].name'`), and names the
  `schema/mapping/wire-fields.json` entry for every new or changed field as
  `column`, `derived` or `client-local` with a note. CANT-141 rev 1 left the
  mapping file out of its footprint entirely; `TestEveryWireFieldIsMapped`
  needs no database and turns plain `go test ./...` red on the first field
  without an entry. A plan that adds a field and does not name the mapping
  entry is blocking.
- **A vector for a behaviour that is not in the schema, or a weakened
  vector, is blocking.** A `reject` case asserts that Go refused, not which
  constraint fired; a `tolerate` case is refused by Go and decoded to
  `unknown` by TypeScript and Dart (CANT-74). A plan proposing a vector that
  proves nothing the existing ones do not (CANT-141 rev 1's group vector,
  redundant with `server_conversation_frame` and `sync_response`) is advisory.
- **Compatibility is the schema header's rule, not the plan's.** The
  `description` at the top of the schema lists what is additive and what
  bumps `x-wire-version`. Read it before accepting "additive": a plan that
  re-scopes what an existing server-computed number counts is additive **only
  if** the field's description says how a client keeps a held value fresh —
  the FRESHNESS clause CANT-140 ruling 3 added after CANT-135 rev 1 called a
  meaning change additive because "the type and bounds do not change". New
  fields are optional-only; a required field on a server-emitted type is
  breaking. Enum direction is computed (CANT-74): open on clients for
  server-emitted enums, closed everywhere for client-authored ones, and
  `unknown` is a reserved spelling.
- **Contract text is drafted in the plan when the building row is
  `evidence`.** Under Mode A nobody reads the diff, so a wire description
  decided while writing the PR is decided by nobody. CANT-141 rev 3 bounced
  for sketching two descriptions a criterion said were "written now"; rev 4
  carried the literal text and the row's `Done when` became text-against-text.
- **Close codes are not the wire.** A new private-range WebSocket close code
  (the `4000`/`4001`/`4002` precedent) is a server change and a row in the
  reconnect table in `docs/decisions/cant-31-refresh-and-terminal-reconnect.md`,
  not a schema change; a plan that calls it one, or omits it from that table,
  has the category wrong.

### 4. Derived rather than stored — blocking where the two could disagree

There is no stored unread count; `first_unread_seq` drives both the badge and
the "N NEW" rule. `head_seq` is served from `conversations.last_seq` and is not
a column. `initials` are computed from `display_name` at serve time. The one
deliberate inverse is waveform peaks, stored and computed server-side because
the seeded generator overflows 2^53.

A plan adding a **stored** column that duplicates something derivable is
blocking unless it says what the two sources are and how they cannot come
apart. Check the neighbours in `wire-fields.json`: if the field beside it is
`derived` with a reason, the new one needs the same reason or the same kind.
A plan moving the peak computation client-side is blocking.

### 5. The client never claims what the server cannot keep — blocking

D1 declines end-to-end encryption; the header reads `TLS`, never `E2E`.
`sending`, `queued` and `failed` are `client-local` in `wire-fields.json` and
not expressible on the wire. A plan putting a delivery, privacy or encryption
claim on the wire that the server does not back — a third rung on
`DeliveryState`, an outbox state on a server frame, a badge string asserting
what `TLS` does not — is blocking. A plan whose client renders a value the
server has stopped backing (CANT-140's `READ 6/7` for a fully-read message
after an offboard) must say so against the `Done when`, not only in a ruling's
consequence line.

### 6. Transcription is a client of the estate ASR service — blocking

One whisper runner serves the estate behind a single-flight GPU lease. A plan
with a second queue, a second scheduler, a retry loop that re-submits on its
own clock, or a direct whisper invocation is blocking. The client is generated
from `asr/openapi.yaml` in the chronicle repository at an `asr-v*` tag — a
neighbouring repository, so the tag's existence is `unverified` from here
(check 1); what *is* checkable here is that the plan generates rather than
hand-writes, and plans against the 60-second model-switch bound rather than
the resident-model throughput number.

### 7. Migrations

- **The next free number is looked up:**
  `ls migrations/*.up.sql | sort | tail -1`. The plan's number is that plus
  one, zero-padded to four. A plan claiming a number that is taken, or
  skipping one, is blocking. The number is as of `main`: two plans in flight
  can both hold it honestly, and the later one renumbers at build time.
- **`.up.sql` and `.down.sql` both.** `loadMigrations` in
  `internal/store/migrate.go` refuses an up with no down, and
  `migrate_test.go` proves the refusal; a plan that says "no down needed" is
  describing a migration the migrator will not load.
- **The guard stays green.** A column that backs a wire field needs its
  `wire-fields.json` entry (check 3); a `derived` entry needs a non-empty
  note; a required wire field over a nullable column needs `required_via` or
  `nullable_because`. `TestWireMapMatchesTheDatabase` checks the types against
  a real database — CI has one, so a plan cannot defer the entry to "the PR".
- **`ON DELETE` never reaches authored messages.** `messages.author_id` and
  `messages.sender_device_id` are `RESTRICT` (`0003_messages.up.sql`), and
  that single fact keeps `CANT-33` off the Mode C list. A plan proposing a
  `CASCADE` from `users` or `devices` toward `messages`, or a `DELETE` against
  `messages` or `attachments` without a bounded predicate, is blocking.
- **`retention_days IS NULL` means inherit-global-infinite, not zero days**
  (`0002_conversations.up.sql`, column comment; `CHECK (retention_days >= 1)`
  keeps `0` inexpressible). A plan whose sweep reads `NULL` as `0` deletes
  everything.
- **A fixture the server cannot serve is a criterion that proves nothing.**
  CANT-141 rev 2 proposed two smoke fixtures making two directs between the
  same pair; `conversations_direct_key_idx` forbids it. Check a proposed
  fixture against the constraints in `migrations/`.
- **Blast radius is stated.** A backfill names its row count's source and its
  lock; "small table" is not a number.

### 8. Locked decisions are not re-litigated by a plan

`README.md` § *The decisions that are closed*: **D1** no E2EE, **D2** Catenary
owns its tokens with Cloudflare Access on `/admin` and metrics only, **D3**
Vue web plus Flutter, **D4** everything is a conversation (the column is
`kind`, CANT-13 ruling 0, though that row says `type`), and transcription as
client two of the estate ASR. Not in that table but equally closed: no
federation, and no public signup — accounts arrive through Purser or a host
command, "never by any signup path" (`internal/store/bots.go`,
`cmd/catenary/user.go`).

A plan that touches one **says so and asks a ruling**; it does not argue the
decision away in prose. A plan that quietly builds on the other side of one —
a new entity that is not a conversation, an encryption claim, a self-serve
enrollment route, a room reached from another instance — is blocking.

### 9. A quoted number is re-derived from where it lives

A plan that quotes a number cites where it lives, and you re-derive it. The
ones plans reach for: `AccessTokenLifetime = 15 * time.Minute` and
`RefreshTokenLifetime` (`internal/store/tokens.go`), `ReuseGraceWindow = 10 * time.Second`
(`internal/store/refresh.go`), `readNotifyCap = 64` (`internal/hub/hub.go`),
the 60-second ASR model-switch bound (`CLAUDE.md`), the vector count
(check 1's `jq`). Grep the constant; redo the arithmetic (CANT-127 rev 1's
~720 links an hour was re-derived from the 5-second dial ceiling before it was
accepted). A rate claimed from code that does not produce it — CANT-129's "once
per device per fifteen minutes, forever", which the catch-up triggers do not
generate on a quiet device — is advisory when the conclusion survives without
it and blocking when it is the argument.

### 10. Criteria are dischargeable by this ticket, with a named method

- **Each criterion is checkable in isolation by its `method`.** A `unit`
  criterion names an assertion a test can fail; one that has become rationale
  (CANT-141 rev 3's criterion 2) or a one-time measurement (its criterion 6,
  an `EXPLAIN` under `unit`) is blocking until it says what fails. `manual`
  says who acts and when. A method the ticket cannot exercise — `migration CI`
  on a plan with no migration (CANT-127 rev 1) — is a mislabel.
- **A regression criterion can be shown failing before and passing after.**
  CANT-140 rev 1's `read_by ≤ conversation.member_count` failed on `main` and
  still failed after the fix; the property the fix establishes was against a
  field that did not yet exist. Ask what "shown failing against `main`" means
  and make the plan say it.
- **Criteria that presuppose a pick are labelled, and every pickable option
  has a row.** A criterion has `approved` and `rejected` and nothing else —
  there is no `not_applicable`. CANT-31 rev 1 had seven of eleven criteria
  describing one option's output; picking otherwise left them with no honest
  verdict. The convention since is a leading `[ruling N → option M]` label,
  and a criterion for every option a person could pick (CANT-140 rev 2's
  option D promised a behaviour no criterion built). A revision after a pick
  erases the pick, so the rows are on the revision that gets approved.
- **Cross-references survive renumbering.** Rulings are **0-based positions**;
  CANT-31 rev 1 counted from 1 throughout and every reference named the ruling
  one to the left. Criteria cited by number range (`0–7`) point at the wrong
  rows after the next revision splits one (CANT-141 rev 3); cite by leading
  words. Prose that says `§3` must say whether it means the plan's section or
  the decision record's (CANT-127 rev 3).
- **Every approved criterion is in some row's `Done when`.** One in no row is
  one the closing evidence never has to show (CANT-141 rev 4 had two,
  including the only integration test of the ticket's server behaviour).

### 11. The work breakdown, and the eight Mode C keys

A Mode B plan proposes its split and the split is approved with it
(`CLAUDE.md` § Mechanics). An empty `work_breakdown` on a `decision` ticket is
blocking (CANT-31 rev 1). Rows' `depends_on` order the deploy: CANT-31 rev 4
had the "bare `1008` is terminal" row able to land before the hello-timeout
code it depends on; CANT-135 rev 2 had row 1's `Done when` need a change that
lived in row 2, which `depends_on: [0]`.

**Mode C is exactly eight tickets, by key:** `CANT-14` (both ordinals in one
transaction — the send path's draw, `internal/store/messages.go`), `CANT-22`
(WebSocket upgrade and hello — `internal/api/socket.go`, `awaitHello`),
`CANT-29` (refresh rotation with reuse detection — `RotateRefresh` and
`RotateRefreshProposing`, `internal/store/refresh.go`), `CANT-63` (edits and
deletes — no home on `main` yet, so its test is the act: any row that rewrites
a message body or deletes from `messages`), `CANT-67` (retention sweep — `internal/store/sweep.go`), and
`CANT-130`, `CANT-134`, `CANT-131` (the door through which a device is
enrolled as an existing person — `EnsurePerson` in `internal/store/persons.go`,
`RedeemEnrollment` in `tokens.go`, `DeactivateUser` in `offboard.go`). A new
ticket inherits the
project default, `evidence`, never its parent's mode. So:

- **A row that edits one of those homes says `review_mode: full` explicitly**,
  and gives the **generating rule** as its reason — *it can destroy authored
  messages, or hand an agent write access to them* — not "the function belongs
  to a Mode C ticket". CANT-31 rev 3 had two rows editing Mode C code with
  contradictory reasons; ownership was not the test, and the plan said so once
  corrected. A row that edits `RotateRefresh` and is left at `evidence` is
  blocking: that function would change with nobody reading the diff.
- **A row claiming `full` for a reason the rule does not give** (CANT-140 rev 1
  read a follow-up bug as a sub-task) is advisory: `full` may still be right
  as a judgement lift, and the plan should say that is what it is, because
  "the list does not grow by habit" is in the same file.
- **A ticket needing more than one PR is split first.** A row that is one PR
  holding a store change and a schema field regenerated into three languages
  buries the lines that matter under generated files (CANT-140 rev 1).

## The reviewer's standing

- **Advisory on a passing check; blocking only for a hole a person would
  bounce.** Style, wording and a better shape are advisory. A wrong invariant,
  a criterion nobody can run, an unmarked either/or, a citation the argument
  rests on — blocking. You spend your own credibility, never the human's
  revision budget: the cap is five by default (`plan_revision_cap`; crossing
  it is `plan_stuck`), and a bounce for a nit costs one of them.
- **The approval half never applies to a plan with human-only rulings.** When
  `plan-context.json` says `live_required_ruling: true`, or `review_mode` is
  `full` or unset, a clean pass means a person is now owed the rulings — not
  that the plan is approved. When the mode is `evidence`, or `decision` with no
  live required ruling, you are the last gate before an agent approval: weigh
  a clean pass accordingly, and never let green mean nothing was reviewed.
- **Never pick a ruling. Never supersede a plan. Never write the check row.**
  Proposing a recommendation is not choosing; say "I would take option N" if
  it helps, and stop there.

## Posture toward rulings

Scrutiny goes to the **recommended option** (`recommended_position`) and to
the criteria, sections and rows written for it. That is the path a person is
being asked to approve, and where a wrong claim costs a revision.

- **Non-recommended options get an honesty check only**: is the option
  described falsely, or strawmanned so the recommendation looks better? CANT-31
  rev 1's ruling 1 option 1 "return the successor rotation minted" could not be
  built — both token types are hashed at rest — and was left pickable; that is
  a finding on an option's honesty and it is blocking, because a person could
  pick it. Anything else on a non-recommended option — costing, criteria,
  spikes to make it decidable — is advisory, tagged "only if option N is
  picked". Do not demand full coverage of paths the plan recommends against.
- **A genuine either/or argued in prose with no ruling attached is blocking.**
  It is the one thing standing between a ruling-less `decision` plan and an
  agent approval. CANT-135 rev 1's "the type and bounds do not change, so
  `x-wire-version` stays 1" was a compatibility decision inside a consequence
  line, and every option carried the same unexamined premise; CANT-141 rev 1's
  "where a direct's title comes from" was one sentence in a frontend section.
  Both became rulings on the next revision.
- **Dependent rulings are gated, not asserted independent.** A ruling that is
  only coherent under another's option uses `gated_by_position` /
  `gated_on_option` (CANT-127 rev 1's cap ruling was only sound behind the
  age-out). "Five rulings, all independent" is a claim you check against the
  criteria's labels.
- **An option nothing builds** — whose body says "its own ticket", or that no
  criterion and no row backs (CANT-140 rev 2's options C and D) — either gets
  a conditional row now or comes off the docket. Advisory when it is not the
  recommendation; blocking when it is.
- **A pick exists (`chosen_option_id` set):** that option is the reviewed path
  and the others are moot. State verdicts against the reviewed path — "clean
  on the recommended path" — so a person can tell what an approval means.
- **The recommended option itself unsound is blocking, said plainly.** You may
  propose a different recommendation; you never pick.

## Exit condition — ready for a human

The automatic loop (SWY-445) reads this list. A CANT plan is ready for a
person when **all** of these hold on the current revision, checkable from the
checkout plus `plan.json`:

1. **No blocking finding on the recommended path** — checks 1–11 above pass
   for the recommended option of every live ruling and for the criteria,
   sections and rows written for it.
2. **Every unmarked either/or has a ruling.** No decision is argued in prose
   without a `rulings[]` entry, and dependent rulings are gated.
3. **Every criterion has a `method` and is dischargeable by this ticket** —
   an assertion a test can fail, a `manual` with an actor, a `review` with a
   text to compare against — and every criterion that presupposes a pick is
   labelled `[ruling N → option M]`, with a row for every pickable option.
4. **The statements each check asks for are present when relevant:** a plan
   touching the send path says where both ordinals are drawn; a plan changing
   a frame or payload names the vectors that move and the `wire-fields.json`
   entries and applies the schema header's compatibility rule; a plan adding a
   stored column says why it is not derived; a plan touching transcription
   says it generates the ASR client and plans against the 60-second bound; a
   plan with a migration names the next free number and both files; a plan
   touching D1–D4, signup or federation says so and carries a ruling.
5. **The work breakdown exists on a `decision` ticket**, every approved
   criterion is in some row's `Done when`, rows editing a Mode C home say
   `review_mode: full` with the generating rule as the reason, and
   `depends_on` orders the deploy.
6. **Every citation the argument rests on has been verified here**, and every
   neighbouring-repository claim is marked `unverified`.

Reaching it means **a person is now owed the rulings**. It does not mean the
plan is approved, and on a plan with a live required ruling it never can.

## Re-reviews

Round three is shorter than round one. On a revision after the first:

- **New blocking findings only.** Do not restate an open thread; do not add
  advisory findings for things the previous revision also had; do not
  re-raise what the author declined with a reason (CANT-141 rev 1's group
  vector was dropped with a reason and stayed dropped).
- **Check a fix landed where the invariant lives, not where it was reported.**
  CANT-141 rev 2 moved a guard onto a `LEFT JOIN … ON c.kind = 'direct'` — the
  rows came back right and the work was unchanged; rev 3 moved it inside the
  lateral's `WHERE`, and rev 4 pinned the placement in a source-shape test so
  the mistake this ticket had made twice could not return. A fix that changes
  the text and not the mechanism is not a fix.
- **Re-verify the fixed claims.** Each prior finding's answer is a new
  citation (check 1). Read `reviews[]` and `threads[]` in `plan.json` for what
  was raised; read the tree for whether the answer is true.
- **Say in one line what got fixed, then move on.** If nothing new is
  blocking, the summary says so and names the recommended path the verdict is
  stated against.
