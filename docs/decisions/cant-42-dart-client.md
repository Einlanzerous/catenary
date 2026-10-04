# CANT-42 — the Dart client: a pure-Dart twin, over real SQLite files

> **The Switchyard plan is the decision of record.** CANT-42 is Mode B and gated on one: **plan rev 2, approved 2026-10-04**, with all five rulings picked by a person. This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts. Every "why" below is one sentence at most; the arguments are in the plan.

The behaviour of the Dart client was not open when this was decided. `cant-24-resume.md`, `cant-31-refresh-and-terminal-reconnect.md`, `cant-36-outbox.md` and `cant-103-conversation-introduction.md` are each rules for **two** implementations, and this is the second. What was open is the five places those records answer in browser terms. Where this file disagrees with one of them about a rule, this file is wrong.

## What was picked

| ruling | outcome |
|---|---|
| where the package lives | **a sibling directory**, `dart-client/` (`catenary_client`); `dart/` is left alone as the generated wire package |
| one SQLite file, or two | **two**, as the outbox record says: `catenary.db` and `catenary-outbox.db` |
| what replaces the Web Lock | **a SQLite-held lock**, safe across isolates and processes |
| how the credential sits at rest | **in the SQLite file**, in the app's private storage, in the clear |
| how the Dart outbox is held to CANT-36's criteria | **a port** of the numbered criteria and their fault table |

Built by **CANT-191** (package, binding, migrator, journal), **CANT-192** (transport core), **CANT-193** (credential layer, lock, decision vectors), **CANT-194** (driver and cohort lanes), **CANT-196** (outbox), **CANT-197** (the driver's outbox commands), and this record.

## 1 · The package, and what it may import

`dart-client/` is `catenary_client`, a module-for-module twin of `web/src/transport` and `web/src/outbox`. It depends on `catenary_wire` by path and never writes a wire type by hand (Invariant 2).

**It imports no Flutter library** — not `package:flutter`, not `dart:ui`, not a Flutter plugin — so a plain `dart` executable can host it (`CANT-46` ruling 0). `test/source_test.dart` holds that, and plants each violation to show it would be refused. Everything a platform has to supply reaches it through a seam (`lib/src/seams.dart`): the clock, the random source, the socket, HTTP, the lifecycle signals, and the directory the files live in. The Flutter app, `app/`, is a third sibling.

The SDK floor is Dart **3.10**, where `dart/` says 3.6: `package:sqlite3` builds its library through a native-asset hook, and hooks are stable from 3.10.

## 2 · The SQLite translation of each browser mechanism

| the browser mechanism | what stands in for it |
|---|---|
| IndexedDB database `catenary` (journal and credential) | the file `catenary.db`: `journal_meta`, `messages`, `conversations`, `users`, `counted`, `credential` |
| IndexedDB database `catenary-outbox` | the file `catenary-outbox.db`: `outbox`, indexed on `(account_id, "order")` |
| `deleteDatabase('catenary')` for obligation 4's discard | a `DELETE` from the journal's tables in one transaction; it cannot reach `catenary-outbox.db`, and it does not reach `credential` |
| a transaction's `oncomplete` as the durability point | SQLite's `COMMIT` on a connection running `synchronous = FULL` |
| an upgrade step appended to `UPGRADES` | a migration appended to an embedded list, applied in process on open (`lib/src/db.dart`) |
| a Web Lock | a small database file per lock name, held in `BEGIN EXCLUSIVE` for as long as the lock is held |
| `BroadcastChannel` re-render across tabs | **not built**: neither target has a second context with a screen. A second context sees another's writes at its next read |
| `navigator.storage.persist()` (`CANT-36` §9) | **no counterpart** — see §6 |

Wire records are stored as the generated codec's JSON beside the columns the store queries by, so a field added to the schema needs no migration here.

A file this code creates is created empty and made `0600` before SQLite opens it. If that fails, the file is deleted rather than left wider.

## 3 · "No ORM, in-process migrator" is extended to Dart

`CLAUDE.md`'s convention — no ORM, no external migration tool, an in-process migrator applying embedded SQL — **was the Go stack's rule before this ticket and is extended to Dart by it.** `package:sqlite3` is the binding; `sqflite` is a Flutter plugin and cannot run under a plain `dart` executable, and `drift` is a query layer and code generator over `package:sqlite3`, which is what the extended rule declines. A database at a schema version this build does not know is refused by name (`DatabaseTooNew`), never opened and guessed at.

## 4 · The lock

A named lock is one database file per name. Holding it is holding `BEGIN EXCLUSIVE` on that file: it excludes every other connection, in another isolate or another process, and is released when the transaction ends, the connection closes or the process dies — the release behaviour of a Web Lock.

**A lock is never waited for on the event loop.** `package:sqlite3` is synchronous, so acquisition is one non-blocking attempt, retried on a timer. A caller that needs the result of someone else's refresh re-reads the persisted credential when its own attempt next succeeds (`CANT-31` §2).

Two rules sit behind it: at most one refresh in flight per credential, and exactly one outbox drainer, holding the lock only while its own session is `ready` (`CANT-36` §8).

## 5 · The journal's generation guard

Two contexts over one data directory are two writers over one journal, and the journal is behind neither lock. What one can break for the other is a wipe. `SqliteJournal` ports `CANT-175`'s guard whole:

- A wipe stamps the journal with a fresh `generation`, in the wipe's own transaction.
- Every other write reads the stored generation inside its own transaction first. A journal wiped under this context refuses the write with `JournalStale` and reloads what it holds in memory from what is stored.
- The cursor written is never below the stored one.
- A refused **live** write is a catch-up trigger; a refused page is retried by the catch-up that issued it, from the stored cursor.
- `skipStaleCatchUp` is the negative control, with the meaning it has in TypeScript.

## 6 · `CANT-36` §9 has no Dart counterpart

The outbox record's `navigator.storage.persist()` refusal line is **not built**. It exists because a browser may evict IndexedDB. The local store here is real SQLite on a real filesystem — one of the reasons D3 chose Flutter over a webview whose storage the OS may evict.

## 7 · How the outbox is held to its criteria

`dart-client/test/outbox_criteria_test.dart` is `web/outbox.test.ts` written a second time: the criteria a headless outbox can be held to — **0, 1, 2, 3, 4, 7, 8, 9, 10, 11, 12 and 15** — each run once clean, where it must pass, and once per named fault from the same sixteen-row table, where it must fail. Criteria 5, 6 and 16 are rendering and belong to the app; 13 and 14 are attachments and are not built (see *What this leaves open*).

Two faults are spelled for a browser and mean this here: `relaxedDurability` is a connection opened without `synchronous = FULL`; `everyTabDrains` and `lockWithoutReady` are a context that drains without the lock, or takes it while its session is not `ready`.

The two real outboxes are also run against each other across a partition: `CANT-46`'s schedule S5, in `CANT-188`.

## 8 · The driver

`dart-client/bin/driver.dart` is the twin of `web/src/transport/driver/driver.ts` and speaks that file's protocol, all eleven commands. The Go rigs run it through the same adapter as the TypeScript driver, with a different command line.

**It is built, not run by `dart`.** `dart run` writes build-hook output to stdout ahead of the first answer, and the protocol allows nothing on stdout but answers. `dart build cli` produces an executable bundled with its SQLite library; `--probe` opens an in-memory database and exits 0, which is how a lane finds out the bundle can load that library before it hands the driver a client. The plan's wording, that a lane fails when "`dart` cannot be executed", is therefore met as "the driver cannot be executed, or cannot open SQLite".

## What this leaves open

- **The holder's re-read timer is not in the plan.** The plan drops `BroadcastChannel` and says a second context sees another's writes "at its next read", but not when the draining context reads. As built, the lock's holder re-reads the outbox store every second (`holderRereadMs`, a parameter of `Outbox.open`); without it an entry composed in a non-holder waits for the holder's next compose, ack or reconnect. Nothing in this epic has two contexts, so nothing yet depends on the number. It is a choice made at build time and is here to be confirmed or overruled.
- **`catenary.db` uses SQLite's default rollback journal, not WAL,** with a 2-second busy timeout. The plan is silent on journal mode.
- **A background isolate is a second context and is not excluded by anything but the guards above.** E7's push handling will run one. The lock and the generation guard are built for it; nothing has exercised it.
- **Attachments in the Dart outbox are not built.** An attachment draft carries no media and the default uploader refuses. `CANT-201` carries it, blocked by `CANT-162`.
- **A `/sync` page in flight across a stale-journal reload lands on the wiped store,** in both transports: `CANT-199`.
