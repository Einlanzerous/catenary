# R4 — one wire schema, two clients, no drift

**Gate:** IDEA-27 · **Status:** cleared · **Measured:** 2026-08-17

> *Exit criterion: schema defined, and both Dart + TS types generated from it, with generation wired into the build rather than run by hand.*

Met, and extended in two places the criterion did not ask for but the risk does — Go is generated from the same schema, and all three are held to shared golden vectors. Reasoning below.

---

## 1. The toolchain finding

IDEA-27 anticipated one possible negative result: *"the toolchain for Dart codegen from this schema format is bad enough to change the format choice"*. That is close to what happened, but the fix is not a different format.

Both candidates were run against the real schema, not a toy:

| Tool | Dart | TypeScript | Verdict |
|---|---|---|---|
| **quicktype 26.0.0** | ✗ | ✗ | Unusable |
| **json-schema-to-typescript** | — | ✓ | Good, but TS-only and types-only |
| **openapi-generator** | not run | not run | Needs a JVM; none installed |

### quicktype erases the union

quicktype is the only tool that targets **both** languages from JSON Schema, so it was the obvious candidate. It unifies `oneOf` branches instead of emitting a tagged union. The fifteen frame types came out as **one class, 30 fields, 28 of them nullable** — in *both* languages, so it is not a Dart backend problem but how quicktype reads the schema:

```dart
class Wire {
  final String? clientInfo;     // ClientHello
  final String? deviceId;       // ClientHello
  final int? resumeFromLogSeq;  // ClientHello
  final DateTime? at;           // Ping / ServerAck / Message
  final Type type;              // the only real discriminator left
  … 25 more, all nullable
}
```

It also **duplicated `Attachment`** into `WireAttachment` and `MessageAttachment` — two different classes, named after where they appeared rather than what they are, with different fields. That is schema drift *inside a single generated file*, which is a striking way to fail at the one job the tool has.

The envelope union is not a detail of this protocol, it *is* the protocol. A generator that erases it produces types where nothing is required, the compiler helps with nothing, and every field access is a null check the author has to reason about unaided — strictly worse than hand-written classes, and worse *silently*.

### json-schema-to-typescript is good, and that is the problem

It preserves every type name and emits real discriminated unions:

```ts
export type ClientFrame = ClientHello | Ping | Pong | ClientSend | ClientRead | ClientTyping
```

Exactly right. But it is TypeScript-only and emits **types with no codecs** — no decode, no encode, no snake_case↔camelCase mapping, no validation.

**So the honest failure mode was not "no good generator". It was the asymmetry.** Pairing a good TS generator with a bad Dart one yields two type systems of different *shape* from one schema — which is the drift R4 exists to prevent, reached more expensively and with a codegen step to make it look solved.

### Conclusion: one generator, three targets

`codegen/generate.mjs` — zero-dependency Node, ~700 lines — walks the schema **once** and emits TypeScript, Dart and Go. Walking once is what makes the three structurally congruent rather than merely all-generated.

It supports exactly the JSON Schema subset this protocol uses and **fails loudly** on anything else, rather than growing toward being a general tool. That is the cost, stated plainly: this file is ours to maintain. It is justified because the alternative is not "use a tool", it is "use two tools that disagree".

---

## 2. What the schema pins that IDEA-23 left open

Writing the schema forced four decisions the spike had not made. Each is recorded here because each is a place two clients would otherwise have invented different answers.

### 2.1 `seq` is per-conversation, so `/sync?after=N` needed a second ordinal

IDEA-23 says both *"a server-assigned monotonic `seq`, scoped per conversation"* and *"`GET /sync?after=N`"*. **Those two cannot both be true for a client with more than one conversation** — a single scalar `after=N` is not well-defined across per-conversation sequences.

A per-conversation cursor *map* fixes the arithmetic and breaks something else: a client returning from six hours offline needs to learn about conversations it has never seen, which by definition are absent from any map it holds. It also grows without bound.

**Resolved as two ordinals, both server-assigned:**

- **`seq`** — per conversation, dense, starts at 1. Ordering, `first_unread_seq`, the "N NEW" rule. What the UI reasons about.
- **`log_seq`** — **server-global** counter, read as a per-account cursor. Sparse, one integer per device. The *only* thing `/sync?after=` takes.

Gaps in `log_seq` are normal and carry no information (you only see your own conversations' rows) — which is only true because the counter is shared across accounts, not per-account. An earlier draft here said "account-global", which reads as a per-account counter and would be dense; corrected on IDEA-27. Gaps in `seq` mean you are missing a message. That division is the whole point, and it is written into both types' doc comments so neither client has to rediscover it. Zulip works this way for the same reason.

**Ratified as path A on IDEA-27, with one rule the first draft was missing:** within a conversation, ascending `log_seq` must imply ascending `seq`. A client discovers by `log_seq` and orders by `seq`, so if they disagree it applies a conversation out of order. That forbids a `bigserial` — it assigns at `INSERT`, not `COMMIT`, so a transaction holding 100 can commit after one holding 101 and a reader past 101 never sees 100 again. Assign both from a single-row counter with `UPDATE … RETURNING` inside the inserting transaction.

**This is the one decision here that is worth a second opinion** — it adds a column and an index. The alternative is a cursor map with a bootstrapping special case, which is worse, but it is your call.

### 2.2 Client-local states must not be expressible on the wire

The UI ladder is `sending → queued → sent → delivered → read → failed`. Only `sent | delivered | read` are server-authoritative. `sending`, `queued` and `failed` describe a message's relationship to *its own outbox*; the server has no opinion on them.

The draft `types.ts` had all six in one `DeliveryState`. The wire enum now has three, so a client cannot report its local state as fact — and the vector `reject_unknown_delivery_state` proves `"sending"` is refused rather than tolerated.

### 2.3 No floating-point number appears anywhere in the protocol

Found by the conformance runner, not by reading. JSON has one number type; Dart has two. A `number`-typed field holding `0` decodes to Dart `double 0.0` and re-encodes as `0.0` — which is not what TypeScript or Go emit for the same field. A value that round-tripped in two languages did not round-trip in the third.

The tempting fix (compare numbers loosely in the runner) would have hidden real precision differences too. So `duration_sec: 62.5` became `duration_ms: 62500` and every number in the protocol is now an integer, exact in all three languages. Clients format for display, which they were doing anyway.

### 2.4 The heartbeat interval is server-driven

R1 needs a ping every 30–45 s. If that constant lived in two clients it would be two constants, and the Flutter one would be found wrong in the field. It is now `heartbeat_interval_sec` in the `ready` frame, alongside `missed_pong_limit` — deployed, not shipped.

---

## 3. The part that actually prevents drift

Generating types from one schema makes the clients agree about **shape**. It does not make them agree about **behaviour**, and behaviour is where two implementations diverge:

- Is an absent optional omitted, or emitted as `null`?
- Does an unknown attachment kind cost you the attachment, or the message?
- Is a malformed timestamp refused, or accepted and sorted wrongly?
- Does an empty `user_ids: []` survive, or get dropped as falsy?

**None of those is a type error.** Codegen alone catches none of them.

So `vectors/vectors.json` holds 41 golden cases, and all three runners read that same file and must produce the same answer:

```
web:    npm run conformance        41/41   (also runs inside npm run smoke)
dart:   dart run bin/conformance.dart      41/41
go:     go run ./cmd/conformance   31 run, 10 skipped
```

The error paths match across languages too, which is a stronger signal than the pass count:

```
TS    Message.seq: Seq must be >= 1, got 0
Dart  Message.seq: Seq must be >= 1, got 0
```

Two cases are worth calling out because they encode judgement, not mechanics:

- **`unknown_attachment_kind_is_dropped`** — an unrecognised attachment drops the *attachment*, not the message. Losing a whole message over an unknown thumbnail is the worse of the two failures, and a client that hard-errored could never ship ahead of a server that adds a type.
- **`ping_from_server`** — the same `Ping` decoded as a *server* frame. `Ping` and `Pong` belong to both unions, which in Dart means one class implementing two sealed types. A naive generator picks a single parent and this case is how you find out.

---

## 4. Wired into the build

```
npm run gen         regenerate all three targets
npm run gen:check   fail if any generated file does not match the schema
npm run build       gen:check → vue-tsc → vite build
npm run smoke       gen:check → 36 render assertions → 41 conformance vectors
```

Verified rather than assumed — appending one comment line to `generated.ts`:

```
$ npm run gen:check   →  STALE: web/src/wire/generated.ts   exit 1
$ npm run smoke       →  exit 1
```

*"A generated file that someone edits by hand is a hand-written file with extra steps"* — the ticket's words, and now a failing build rather than a convention.

---

## 5. Known gap, stated rather than buried

**The Go decoders do not enforce the schema's constraints.** `encoding/json` cannot distinguish an absent required scalar from an explicit zero one without a generated `UnmarshalJSON` that shadows every required field with a pointer.

So the Go runner executes the 31 roundtrip/ignore cases and **skips the 10 reject cases**, printing each skip by name rather than passing quietly.

This is the right thing to fix first in P1, because the server is the trust boundary — it is the implementation that most needs to refuse bad input, and the only one that currently would not. It is mechanical generator work, not a design question. It is out of scope here only because R4's exit criterion is Dart + TS, and Go was already a bonus.

---

## 6. Layout

```
schema/
  catenary.wire.v1.schema.json   the contract — the only hand-written definition
  codegen/generate.mjs           one walk, three targets
  vectors/vectors.json           41 golden cases, shared by all three runners
  FINDINGS.md                    this file
web/src/wire/generated.ts        generated · 1117 lines
web/conformance.ts               TS runner
dart/lib/src/generated.dart      generated · 1442 lines
dart/bin/conformance.dart        Dart runner
server/internal/wire/generated.go generated · 1171 lines
server/cmd/conformance/main.go   Go runner
```

`web/src/types.ts` — the hand-written draft — is still in place and still used by the mock store. Migrating the components onto the generated types is P1 work and deliberately not done here: it would touch every component for no gain while there is still no transport, and this gate is about the contract existing before either client encodes its own opinion of it, which it now does.
