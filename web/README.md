# Catenary — web client

The **Catenary Web Client** design canvas, realised against a live server. Vue 3 + TypeScript + Vite, per **D3**.

```
npm install
npm run dev        # http://localhost:4009, proxying the API to CATENARY_DEV_API (default http://127.0.0.1:4012)
npm run smoke      # the headless render against a live, seeded server, then conformance and the outbox
                   # (needs Go and CATENARY_TEST_DATABASE_URL — see below)
npm run outbox     # the outbox's criteria, and every named fault required to fail one (node:test)
npm run typecheck
```

## What this is, and what it is not

There is no mock data (CANT-39). Every record on screen came from a server: `main.ts` reads the enrolled credential, `startSession` in `store.ts` starts the transport over it with the durable IndexedDB journal, and CANT-35's projection puts the journal into `state`. A device with no credential starts nothing and opens on the login form; a login or a re-enrollment restarts the session (`onEnrolled` in `account.ts`).

`npm run dev` needs a `catenary serve` to talk to. Vite proxies `/ws`, `/sync`, `/enroll`, `/refresh`, `/devices` and `/conversations` to it with `changeOrigin` off, so the browser only ever talks to its own origin — the WebSocket door refuses an `Origin` that is not the request's `Host`, and that check is not loosened for a dev server.

`npm run smoke` renders against a live server too. `cmd/catenary/websmoke_test.go` resets `CATENARY_TEST_DATABASE_URL`, seeds the canvas's corpus through the store, serves it through the same composition root `catenary serve` runs, and runs the built `dist-smoke/smoke.js`, which logs in through the real `/enroll` and renders what its transport caught up on. Without a database it refuses rather than skips; `verify.sh` runs it in its database lane.

## Layout

| | |
|---|---|
| `src/styles/tokens.css` | The token contract. Names are shared with the Flutter client; values are per-theme. Dark is primary, light derived. A raw hex in a component is a bug. |
| `src/types.ts` | Wire types — **hand-written, and temporary**. R4 says these are generated from one schema alongside the Dart equivalents *before either client is written*. This file is a draft of that contract, not the contract. |
| `src/store.ts` | One `reactive` object. Not Pinia; the dependency list stays at `vue`. |
| `src/transport/` | The WebSocket transport (CANT-35): session loop, heartbeat, CANT-31 §4 close table, backoff, `/sync` catch-up and the in-memory journal, and the credential layer (CANT-152: CANT-31 §1–§6 refresh, the chain, CANT-127's hold and CANT-129's refused wait, over IndexedDB database `catenary` under a per-credential Web Lock), with no Vue import and every clock, socket and timer injected. Each module names the `internal/client` file it mirrors. `npm run test:transport` runs its unit tests; `startSession` in `store.ts` wires it into the app. |
| `src/lib/waveform.ts` | Renders stored peaks. `peaksFromSeed` reproduces the canvas generator for the recording animation only — see the portability note in that file. |
| `src/components/` | One component per object in the canvas. |
| `src/outbox/` | Unacked sends (CANT-36, rules in `docs/decisions/cant-36-outbox.md`): the `OutboxStore` and `OutboxTransport` seams, the state machine, the render projection, IndexedDB database `catenary-outbox`. |
| `smoke.ts` | SSRs the app, caught up from a live server, and asserts the canvas's landmarks are actually on screen. |
| `outbox.test.ts` | `npm run outbox`: each outbox criterion as written, then each named fault, which must make its criterion fail. |

## Sections covered

01 main view · 02 composer and connection states (recording, offline/queued, failed send, resync, typing) · 03 voice note, all three transcript states · 04 image message · 05 search over text and transcripts · 06 replies, four source types plus jump-to-source · 07 logomark. Narrow layout below 900px. Both themes.

**The logomark** (section 07) is form 1C "Span" — two masts and the messenger wire's sag, the catenary curve itself, knocked out of a solid tile. `Logomark.vue` draws it on the canvas's 48-grid mask and fills the tile from `--accent-mark`, a token of its own (`#E5A03C` dark, darkening to `#C4761F` on light so the cutout stays visible): the brand is not a state, so it does not compete with the one live conductor. `public/favicon.svg` is the same geometry, and the one raw hex in the client, because a favicon cannot read a stylesheet.

Two things the canvas is specific about and it is easy to get wrong:

- **The header chip reads `TLS`, not `E2E`.** D1 declines end-to-end encryption, so the chip states the guarantee that actually holds.
- **The search shortcut is `CTRL+K`**, and the keyboard handler binds Ctrl only — no `⌘`. iOS and macOS are out of scope, and a second undocumented binding is how two clients start to disagree.
- **Typing is the thread's own last row** (call 10a), in the message column, not a strip under the composer — so the composer stays the bottom of the window. Its naming rule lives in `store.ts` because Flutter has to match it: one person is a first name, two or three are comma-separated in the order they started, four or more become "Several people".

## Four things derived rather than copied

The canvas is a mock of one moment; an app has to be right at every moment.

1. **Timestamps are relative to today**, not pinned to 15 AUG. The rail's vocabulary is relative — clock, weekday, date — so fixed dates make every row read as stale within a week and dim the whole list.
2. **Unread counts exclude your own messages.** You cannot have an unread message you sent. The canvas confirms it: "3 NEW" sits above a run of five, three of them from other people. There is no stored count at all — the badge and the rule both derive from `firstUnreadSeq`, so they cannot disagree.
3. **Rail previews and stamps come from the last message**, so the rail shows 14:41 where the canvas drew 14:12 — the canvas's own replies section runs later than its rail.
4. **Transcript word counts are derived** from the text on screen, so "EXPAND · 96 W" can never lie.

## Two places the canvas disagrees with itself

Each is resolved in code with a comment naming the choice.

- **READ in the accent** (call 03 + the status ladder) vs **READ in meta grey** (all four status labels the canvas actually renders, plus the rail). Follows the renderings, four against one. One line in `StatusLabel.vue` flips it.
- **"Read rows dim their name"** (call 05) vs the markup, which dims exactly the four rows whose stamp is not a clock — i.e. *quiet*, not *read*. Follows the markup, so Sunday Dinner and Nadia stay bright. → `ConversationRow.vue`

## Open, from the canvas

Room avatars (none today; may need 2-letter tiles past ~40 rooms), and whether the VOX zebra lift in search subtly privileges voice results — call 19 flags it as debatable. Deleting `.hit.vox` in `SearchView.vue` flattens it.
