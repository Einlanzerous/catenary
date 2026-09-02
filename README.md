# Catenary

A self-hosted chat service for a small trusted group. Text, images, voice notes and server-side transcription of them, at parity across web, Linux desktop and Android; iOS deferred until there is hardware and a willing collaborator.

> *Catenary:* the overhead wire that delivers continuous power to electric rail. Sits alongside Switchyard.

**The core idea: this is an append-only log with a sync protocol, not a chat app.** Every message gets a server-assigned monotonic `seq`, scoped per conversation, plus a server-global `log_seq`; clients send with an idempotency key and resync with "everything after N". Offline queueing, strict ordering, multi-device convergence and crash recovery all fall out of that one decision, and everything else is features on top of it.

Graduated from **IDEA-23** with all six P0 gates cleared on measured evidence, on real hardware and through the real edge. Tracked in Switchyard under **CANT** — 10 epics, 60 tickets. Start with `CLAUDE.md` for the invariants and the working agreement.

## Status

Graduation has landed: the project, the board and this repository exist. **No server has been built yet** — E1 is the spine, and the tree here is the P0 evidence plus the wire contract and the web client that the spike produced.

Run everything checkable without hardware:

```bash
./verify.sh
```

## Layout

| path | what |
|---|---|
| `CLAUDE.md` | **the invariants and the working agreement — read this first** |
| `SPIKE-RESULTS.md` | the P0 summary: all six gates, and the evidence behind each |
| `verify.sh` | one command, every offline check |
| `schema/` | **the wire contract** — schema, generator, 41 conformance vectors |
| `web/` | Vue 3 client against mock data, plus the generated TS |
| `dart/` | generated Dart wire package + its conformance runner |
| `server/` | generated Go wire package, the R1 rig, and the Go conformance runner |
| `spike/r1-websocket/` | **R1** — tunnel rig, kill/resume test, results |
| `spike/r2-push/` | **R2** — FCM probe, collector, per-send latency data |
| `spike/r3-whisper/` | **R3** — whisper.cpp benchmarks and build recipes |
| `spike/r6-purser/` | **R6** — Purser connector stub and its findings |
| `spike/r2-r5-push-and-distribution/` | **R2 + R5** — the joint decision memo |

Each gate directory has its own `FINDINGS.md`. `schema/FINDINGS.md` is the longest and the most consequential, because R4's output is what every later phase is built on.

## The decisions that are closed

Recorded here so they are not re-litigated when the work gets hard. The reasoning is in `CLAUDE.md` and on IDEA-23.

| | |
|---|---|
| **D1 — E2EE** | **Declined**, signed off 2026-08-17. Buys server-side search, server-side transcription, trivial device-add, and history that survives a phone hitting a lake. Mitigations — TLS, encrypted disk, tight backup handling, honesty about what the server can see — are now the whole security story. |
| **D2 — Auth** | Catenary owns its own tokens: short-lived access, per-device rotating refresh. Cloudflare Access stays in front of `/admin` and metrics **only**. |
| **D3 — Clients** | Vue on web, Flutter on Android and Linux desktop. Flutter web draws text as pixels, which is disqualifying for an app about reading and copying text. |
| **D4 — Data model** | Everything is a conversation. A DM is `type = 'direct'` with two members; a room is `type = 'group'`. |
| **Transcription** | Client two of the shared estate ASR service, never a second whisper queue. |

## The six gates, cleared

| gate | verdict |
|---|---|
| **R1** WebSocket through the tunnel | **cleared** — 1h20m idle at 9 ms RTT, `kill -9` resumed with zero loss and zero duplication |
| **R2** push to a cold Doze device | **cleared** — 60/60 across two vendors over ~6 h, p50 0.86 s / 0.64 s, worst 6.07 s |
| **R3** whisper.cpp on the R9700 | **cleared** — 60 s note in 1.0 s on Vulkan; HIP rejected at 4.8× slower |
| **R4** one wire schema, two clients | **cleared** — TS + Dart + Go from one generator, 41 shared vectors |
| **R5** Android distribution | **cleared** — Play internal testing, on Argosy and Lyceum's precedent |
| **R6** Purser connector fit | **cleared** — `connector.Connector` accepts Catenary unmodified |

## Configuration

Env-only, `CATENARY_`-prefixed, no config files. `catenary --help` is the copy of record; a test asserts it names every variable `config.Load` reads, so the two cannot drift.

| variable | default | |
|---|---|---|
| `CATENARY_DATABASE_URL` | — | pgx DSN. Falls back to `DATABASE_URL`. **Required.** |
| `CATENARY_PORT` | `4012` | Next free in the estate's 40xx block, read off `construct-server`'s compose: 4000-4009 are allocated, 4010 the Signet host daemon, 4011 the shared ASR service. Catenary will most likely publish no port at all — like Chronicle and ASR it sits behind Traefik on `construct_net` — so the number only has to be unique to be legible. |
| `CATENARY_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `CATENARY_LOG_FORMAT` | `json` | `json` \| `text`. JSON by default because Datadog parses it into attributes with no pipeline config and Dozzle renders it fine. |
| `CATENARY_SHUTDOWN_GRACE` | `20s` | In-flight grace on SIGTERM. |

Credentials never live in a compose file — they come from Signet.

### The two probes answer different questions

`GET /healthz` is dependency-free: if it answers, the process is alive. `GET /readyz` reports whether this instance can serve traffic. They are two routes because collapsing them would make a Postgres restart restart every Catenary instance, turning a recoverable dependency outage into a reconnect storm across every open socket.

**`/readyz` does not yet check anything.** The pool is not opened until CANT-13, so today it answers 200 unconditionally and no state of the database produces a 503. **Do not point a load balancer's readiness check at it yet** — an instance whose Postgres is down would stay in rotation and fail every request it took, which is the outcome splitting the probes exists to prevent.

Successful probe requests log at `debug` — they are polled forever and would otherwise be the log by volume. A probe answering 4xx or 5xx keeps its level, because a `/readyz` 503 is the most important line this service emits.

## Toolchain notes

The Dart SDK is not on `PATH` by default. `verify.sh` looks in `~/tools/dart-sdk/bin`; override with `DART=/path/to/dart-sdk/bin`.

whisper.cpp lives in `~/tools/whisper.cpp` with two builds, `build-vk` (use this) and `build-hip` (kept for comparison). The Vulkan build needs `LD_LIBRARY_PATH` pointing at the LunarG SDK's `lib` — see `spike/r3-whisper/FINDINGS.md`. **Note that Catenary no longer runs whisper itself**: R3's numbers are inherited by the estate ASR service, and the rig here is kept as evidence rather than as a dependency.
