# Catenary — web client

First-pass implementation of the **Catenary Web Client** design canvas, against mock data. Vue 3 + TypeScript + Vite, per **D3**.

```
npm install
npm run dev        # http://localhost:4009
npm run smoke      # headless render + logic checks (29 assertions)
npm run typecheck
```

## What this is, and what it is not

This is the design realised as a working client. It is **not** the Catenary project: there is no repo, no `CANT` project key, and no server. **IDEA-23** is a spike whose six P0 gates — R1 WebSockets through the tunnel, R2 push, R3 whisper.cpp, R4 one wire schema, R5 Android distribution, R6 Purser connector fit — decide whether Catenary gets built at all. Graduating before those clear is the failure mode the spike names explicitly.

So: no transport, no auth, no persistence. `src/mock/fixtures.ts` holds the canvas's corpus, `src/store.ts` walks messages up the delivery ladder on a timer, and a **HARNESS** strip in the bottom-right switches connection state and theme because there is nothing real to disconnect from yet. Delete `DevToolbar.vue` the day a transport lands.

## Layout

| | |
|---|---|
| `src/styles/tokens.css` | The token contract. Names are shared with the Flutter client; values are per-theme. Dark is primary, light derived. A raw hex in a component is a bug. |
| `src/types.ts` | Wire types — **hand-written, and temporary**. R4 says these are generated from one schema alongside the Dart equivalents *before either client is written*. This file is a draft of that contract, not the contract. |
| `src/store.ts` | One `reactive` object. Not Pinia; the dependency list stays at `vue`. |
| `src/lib/waveform.ts` | Renders stored peaks. `peaksFromSeed` reproduces the canvas generator for fixtures only — see the portability note in that file. |
| `src/components/` | One component per object in the canvas. |
| `smoke.ts` | SSRs the app and asserts the canvas's landmarks are actually on screen. |

## Sections covered

01 main view · 02 composer and connection states (recording, offline/queued, failed send, resync, typing) · 03 voice note, all three transcript states · 04 image message · 05 search over text and transcripts · 06 replies, four source types plus jump-to-source. Narrow layout below 900px. Both themes.

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
