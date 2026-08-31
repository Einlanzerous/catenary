# Catenary P0 — where the six gates stand

**IDEA-23** · one overnight session, 2026-08-17 · all evidence in-tree

| gate | | verdict | evidence |
|---|---|---|---|
| **R1** | WebSocket through the tunnel | **CLEARED** | `spike/r1-websocket/FINDINGS.md` |
| **R2** | Push to a cold Doze device | **CLEARED** | `spike/r2-push/FINDINGS.md` |
| **R3** | whisper.cpp on the R9700 | **CLEARED** | `spike/r3-whisper/FINDINGS.md` |
| **R4** | One wire schema, two clients | **CLEARED** | `schema/FINDINGS.md` |
| **R5** | Android distribution | **CLEARED** — on precedent | `spike/r2-r5-push-and-distribution/FINDINGS.md` |
| **R6** | Purser connector fit | **CLEARED** | `spike/r6-purser/FINDINGS.md` |

**All six cleared.** IDEA-23 says plainly that *"R1 and R2 are the ones that matter"* — both are now answered on measured evidence, on real hardware, through the real edge.

Nothing measurable and nothing undecided is left. **D1 is signed off** (no E2EE), the `seq` / `log_seq` split is **ratified** as path A, and **D3 is settled** as Vue on web plus Flutter on mobile/desktop. Graduation to a `CANT` project and repo is deliberately the author's call, not something this spike does on its own.

All six subtasks are transitioned to Closed/done, each with its evidence on the ticket. IDEA-23 itself is In Progress rather than closed, because closing the spike means deciding whether to graduate — and that is a decision, not a measurement.

```bash
./verify.sh     # every offline check, one command
```

---

## What each gate came back with

### R1 — the socket survives, and a forced kill resumes clean

Both halves met, through a **real Cloudflare tunnel**, not localhost.

```
[hb]   DONE total=1h20m0s reconnects=0  longest_single_socket=1h20m0s
[nohb] DONE total=1h20m0s reconnects=35 longest_single_socket=2m5s
```

The heartbeat client held **one socket for 1 h 20 m**, at a flat 8–12 ms RTT across ~137 ping/pong exchanges. It was closed by the test's own deadline expiring, not by the network.

Heartbeats prove a socket is *alive*; they do not prove it still *delivers*. A second run settled that by publishing only after the socket had already been open for an hour — **3/3 delivered nine seconds past the one-hour mark, over the same socket, zero reconnects.**

The control is what makes that mean something: **35 sockets, every one lasting 2 m 05 s.** Not "about two minutes" — the same value 35 consecutive times. That is an edge timer, not a flaky network.

Forced kill (`kill -9` mid-stream, 10 messages published while dead):

```
server messages : 20     DUPLICATED : 0     LOST : 0     PHANTOM : 0
RESULT: PASS — zero loss, zero duplication
```

Replaying the same idempotency keys created no second message. And the missed-pong severance was exercised directly, with server-side fault injection that goes deaf while holding the connection open — the only way to reproduce the silent half-open socket that is the real risk. That put a number on something previously unstated: **at a 35 s heartbeat, a client can believe it is connected for 70–105 s after it is not.** Under the ~100 s idle window, which is what the interval was for, but it is the interval's real cost and it is a server-driven dial rather than a constant.

### R3 — Vulkan, 60 s note in 1.0 s

**59.6× realtime** with `small.en`, including the Opus decode.

The interesting part is a mistake worth keeping: the first pass built ROCm/HIP, found it correct — identical transcripts, 89% GPU utilisation, zero build errors — measured 14× realtime, and recommended it. Building Vulkan afterwards showed it is **4.8× faster**, and that the HIP numbers had also inflated the CPU baseline by carrying ~3.2 s of ROCm initialisation into a measurement of the *CPU* path.

> **A backend that works and is 5× off the pace is a failure mode worth naming, because nothing about it looks like a failure.** The first pass checked correctness and checked the GPU was genuinely used, and never checked whether the number was good.

| model | vulkan | hip | cpu |
|---|---|---|---|
| base.en | **777 ms** | 4044 ms | 2683 ms |
| small.en | **1007 ms** | 4178 ms | 7298 ms |
| medium.en | **2045 ms** | 5281 ms | 25595 ms |

For `base.en`, **HIP is slower than the CPU**. Use Vulkan; use `small.en`; `medium.en` is now cheap enough to be a real option.

### R4 — one schema, three languages, 41 shared vectors

```
web:    41/41    dart: 41/41    go: 31 run, 10 skipped
```

**The toolchain finding.** quicktype — the only tool targeting both Dart and TS from JSON Schema — **unifies `oneOf` branches instead of emitting a tagged union**. The fifteen frame types came out as one class with 30 fields, 28 nullable, in *both* languages. `json-schema-to-typescript` is genuinely good but TypeScript-only and types-only. **The finding is the asymmetry, not the absence**: a good TS generator plus a bad Dart one gives two type systems of different *shape* from one schema, which is the drift R4 exists to prevent. So one zero-dependency generator walks the schema once and emits TS, Dart and Go.

**Vectors matter more than the codegen.** Generated types make the clients agree about *shape*; they say nothing about whether an absent optional is omitted or nulled, whether an unknown attachment costs the attachment or the message, whether a malformed timestamp is refused. None of those is a type error. 41 golden vectors, read by all three runners, are what pin the behaviour — and the error *paths* match across languages, which is a stronger signal than the pass count.

**Four things the schema had to decide** that IDEA-23 left open — the `seq` / `log_seq` split is the one worth your review, and is called out on IDEA-27.

### R6 — the abstraction holds

`connector.Connector` accepts Catenary with no widening, no extra method, no side-channel. Seven tests against the **real** interface.

The thing worth keeping: **Deprovision's fan-out makes it non-atomic upstream, so the order is load-bearing.** Revoke devices first and fail partway, and you are left with an *active* account and no way for Purser to tell a half-completed offboard from one that never started — the offboard failed open. Disable the account first and the same failure leaves access genuinely gone, truthfully reported, and safe to retry. Not derivable from a design review; that is why the gate asked for a stub.

### R2 — push clears on two vendors

**60 sends, zero misses, over ~6 hours**, on hardware, unplugged, in verified deep Doze.

| device | delivered | p50 | p90 | max |
|---|---|---|---|---|
| Pixel 9 Pro (stock Android 16) | **31/31** | 0.86 s | 1.21 s | **6.07 s** |
| Samsung SM-X230 (One UI, Android 16) | **29/29** | 0.64 s | 0.92 s | **1.48 s** |

The Pixel ended the soak in genuine deep Doze with the probe decayed to standby bucket **40 (RARE)** — aged out of every favourable bucket and still receiving. The Samsung was *tighter* than the Pixel at every percentile, so the vendor-optimiser concern that motivated testing it did not materialise.

The 6.07 s outlier is the number worth remembering: still "within seconds" and nowhere near the 4-minute failure the ticket warns about, but 6× the median. An occasional multi-second delay under deep Doze is normal on stock Android. Fine for chat; not fine for anything claiming real-time.

**Decision: FCM, data-only, `priority: HIGH`, no message content in the payload** — a wake signal, with the client fetching through `/sync?after=<log_seq>`.

The gate cost far more time than the measurement warranted, and **all of it was harness faults that made a working system look broken** — a callback whose errors were swallowed, a debug tool that bypassed the very bookkeeping being read as evidence, and Samsung's suppression of app logcat. Recorded in full in `spike/r2-push/FINDINGS.md`, because the pattern is the lesson:

> Treat "the measurement says zero" as a claim about the instrument until proven otherwise. This project produced that failure three times.

### R5 — cleared on precedent

Friends install via the Play Store as a matter of policy, and **Argosy and Lyceum already ship to them over the Play internal testing track** — the exact path Catenary would use. The criterion is already demonstrated by two apps with real recipients who did not build them.

Two findings made the alternative moot anyway: Google's **Developer Verification** (unverified sideloading now costs the recipient a reboot and a 24-hour wait) and **Samsung's Auto Blocker**, on by default, which blocks installs from unauthorized sources *and* adb — found on the actual test hardware, not in documentation.

---

## What I would do next, in order

1. **Graduate, or say why not.** Every gate is clear and every decision is closed, so a `CANT` project and a repo is now a formality rather than a judgement. The build plan — ten epics, sixty tickets, each with a model tier — is on IDEA-23.
2. **Land the cost instrument first.** SWY-223 (LLM Insights fed from Claude Code) is the stated gate on starting in earnest. Two of its open questions become load-bearing here: ticket attribution has to exist *before* the first epic, and it delivers tokens rather than dollars.
3. **Adopt the house codegen pipeline where it fits.** Argosy already runs one OpenAPI spec → Go + TS + Dart. It covers Catenary's REST surface cleanly and pinches only on the WebSocket envelope union.
4. **Close the Go validation gap** from R4 — the server is the trust boundary and is currently the only implementation that would not refuse bad input.

## What I deliberately did not do

- **No transitions, no new tickets, no repo, no `CANT` project.** Graduating early is the failure mode IDEA-23 names, and two gates are open.
- **No Flutter client.** IDEA-27 comes first, and now that it has landed the Flutter client has a contract to be generated against rather than a second hand-written copy of one.
- **Nothing written into the Purser repo.** The R6 stub compiles against Purser's real interface from the Catenary tree via a module path and a `replace`.
- **The web client still uses its hand-written `types.ts`.** Migrating the components onto the generated types is P1 work; doing it now touches every component for no gain while there is still no transport.
