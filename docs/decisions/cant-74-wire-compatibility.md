# CANT-74 — wire_version compatibility: open enums on the clients, closed on the server

> **The Switchyard plan is the decision of record.** It is versioned, its criteria carry verdicts and its rulings carry picks, and it gates the ticket: **Switchyard `CANT-74`, plan rev 3, approved** (Switchyard is estate-internal, so there is no link that resolves from a clone of this public repository). This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale.

The policy itself is in the wire schema's header (`schema/catenary.wire.v1.schema.json`, `description`), which every client reads and the emitted `openapi.yaml` carries verbatim. The generator enforces it; `schema/codegen/generate.test.mjs` watches each rule fire; the conformance vectors pin what each side does.

## Rulings, as settled

| | settled |
|---|---|
| **0 · which shape carries forward compatibility** | **Open enums on the clients, closed on the server.** Server-side gating by session `wire_version` was rejected: it cannot reach `/sync`, and REST negotiates no version. `x-wire-version` stays 1 and is reserved for breaking changes. |
| **1 · what an unknown client-open value decodes to** | **A reserved sentinel `unknown`**, with the raw string reported once per (enum, value) per process. TypeScript adds `'unknown'` to the literal union; Dart adds an `unknown` member to the enhanced enum; encoding emits `"unknown"`. No enum may define `unknown` as a real value. |
| **2 · how `vectors.json` says the answer differs by side** | **One new word, `expect: "tolerate"`.** The TypeScript and Dart runners treat it as `roundtrip` against `encoded`; the Go runner treats it as `reject`. Each runner states its side once in its header. |
| **3 · `ServerReady.wire_version`** | **Yes.** Optional, additive, one roundtrip vector. The client's report can say how far apart the two ends are. |

## What the tree now enforces

Direction is computed from two named root lists in the generator — `SERVER_ROOTS` and `CLIENT_ROOTS` in `schema/codegen/generate.mjs`, deliberately not re-listed here: a copied enumeration is the drift this file's preamble refuses, and this sentence had already gone stale on two client roots before CANT-117 added a server one. `node schema/codegen/generate.mjs --classify` prints the current split, and `generate.test.mjs` pins it. On today's schema:

| enum | treatment |
|---|---|
| `DeliveryState`, `TranscriptState`, `ReplyRefKind`, `ErrorCode`, `ConversationKind`, `ResyncReason` | client-open |
| `TypingState`, `OutboundAttachment.kind` (inline, client-authored) | closed everywhere |

Seven non-enum types are reachable from both sides — `Uuid`, `Timestamp`, `Seq`, `LogSeq`, `Token`, `Ping`, `Pong` — and that is legal; the rules classify unreferenced types and enums only.

Four lints fail the build: an unreferenced `$defs` entry in neither root list; an enum reachable from both sides; an enum defining `unknown`; an inline enum on a server-emitted type. `ServerResyncRequired.reason` was promoted to the named enum `ResyncReason` under the last of these, with no change to the bytes on the wire.

What a client renders for `unknown` is the known value whose rendering claims least: `DeliveryState` → `sent`, `TranscriptState` → `pending` with no progress affordance, `ReplyRefKind` → `text`, `ConversationKind` → `group`, `ErrorCode` → `internal` honouring `retryable`, `retry_after_sec` and `message` as sent, `ResyncReason` → `cursor_too_old`. The arms live in the client code beside the switch; the compiler demands them in Dart, and in TypeScript via `assertNever` or a `Record` table.

The vector count moved 48 → 56: seven new, one re-kinded (`reject_unknown_delivery_state` → `tolerate_unknown_delivery_state`), and the Ruling 3 case.

## The parse test, and what it measured

`server/spec/openapi_test.go` loads the emitted `openapi.yaml` through kin-openapi pinned at **v0.135.0** — Argosy's pin — and validates it, under `./verify.sh`. That is the committed parse CANT-12's PR #3 re-review left open.

Its two negative controls measured something the plan did not expect. The plan said the parse would catch an unrewritten `#/$defs/` reference and probably not a stray non-`x-` keyword. **It catches both**: the reference is refused at load (the fragment cannot resolve), and the stray keyword is refused at validate as an "extra sibling field", with the keyword named. Recorded on **CANT-105**, whose allow-list now buys an error at emit time naming the schema path, rather than being the only guard.

## Stated here, built elsewhere

- **CANT-22** implements the hello check: accept a `wire_version` the server can speak — today exactly 1 — and otherwise answer `wire_version_unsupported`, `retryable: false`, and close. The minimum-supported constant lands there, with its first use.
- **REST** carries no version and needs none until a breaking bump; the bump's change gives it one.
- **CANT-76** (`User.kind`) is the first consumer: a named `$defs` enum, client-open by construction, rendering `unknown` as `person`.
- **CANT-135** narrowed the meaning-change trigger, and the amended bullet is in the schema header as always: it means a value a client branches on, and re-scoping a server-computed aggregate is additive when the descriptions change with it and no client can be misled. `Conversation.member_count` and `Message.read_by` were re-scoped to active members at wire version 1 under that reading, picked by a person on plan rev 2; the reading that it IS breaking, and what a bump would have cost, is preserved in that plan's *compatibility* section.
- **CANT-140 ruling 3** narrowed that bullet again, after its own bug showed "no client can be misled" was a judgment call: a re-scoping is additive when the descriptions update **and the description names how a client keeps a held value fresh** — where a re-emission reaches it, what a catch-up does not, what a bootstrap does. `DeliveryState`'s FRESHNESS paragraph is the model; a re-scoping whose description cannot say that is breaking. Picked by a person on plan rev 2; the reading that CANT-135's re-scoping was breaking after all, and what a bump would have cost, is priced in that plan's ruling 3 option C.
- **CANT-146** applied that reading once, in the weakening direction: `DeliveryState`'s cap sentence used to promise that a deactivation's span within the cap is re-emitted live, room by room, and the server stopped being able to keep that once an offboard's per-room payloads had to share one author's outbox. The sentence now says the cap for a deactivation or a reactivation is one budget across every room the client shares with that person, that a room reached after it gets no live re-emission however small its span, and that only a bootstrap is authoritative. Additive under ruling 3 because the description names what the client does about it; `x-wire-version` stays 1. A description that had kept the per-room promise would have been the server claiming something it cannot keep.
