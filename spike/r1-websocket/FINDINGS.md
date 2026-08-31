# R1 — a WebSocket that survives the Cloudflare tunnel

**Gate:** IDEA-24 · **Status:** cleared · **Measured:** 2026-08-17

> *Exit criterion: a socket survives 60+ minutes idle through the real tunnel — not localhost, not a direct connection bypassing Cloudflare — and a forced kill resumes with zero loss and zero duplication, verified by comparing the client's message set against the server's log.*

Both halves met, through a real Cloudflare tunnel.

| | result |
|---|---|
| Longest single socket, idle | **1 h 20 m 00 s**, zero reconnects |
| Round-trip time, throughout | **8–12 ms**, flat |
| Delivery over a >1 h old socket | **3/3**, same socket, no reconnect |
| Control (no heartbeat) | died at **2 m 05 s**, **35 times out of 35** |
| Forced kill (`kill -9`) | **zero loss, zero duplication**, correct order |
| Idempotency replay | **no duplicate created** |

---

## 1. The idle test, and its control

Two clients ran side by side for 80 minutes against the same server through the same tunnel. They differ in exactly one thing: whether the application-level heartbeat is running.

**A test without the control would have proved nothing.** A socket surviving an hour tells you the heartbeat works only if you know what happens without it.

### With the heartbeat — 35 s ping, 2 missed pongs to sever

```
00:44:21 [hb] ready · heartbeat=35s · missed_limit=2
00:49:21 [hb] alive 5m0s      · outstanding_pings=0 · last_rtt=8ms
01:14:21 [hb] alive 30m0s     · outstanding_pings=0 · last_rtt=9ms
01:44:21 [hb] alive 1h0m0s    · outstanding_pings=0 · last_rtt=9ms
01:59:21 [hb] alive 1h15m0s   · outstanding_pings=0 · last_rtt=9ms
02:04:20 [hb] socket closed after 1h20m0s: context deadline exceeded
02:04:20 [hb] DONE total=1h20m0s reconnects=0 longest_single_socket=1h20m0s
```

`context deadline exceeded` is the client's own `-run-for 80m` deadline firing. **The socket was closed by the test ending, not by the network.**

Round-trip time never moved off ~9 ms across 80 minutes and ~137 ping/pong exchanges. Every ping was answered — the socket was demonstrably alive and bidirectional right to the end, not merely un-closed.

### And the aged socket still carries messages

Heartbeats prove liveness; they do not prove the socket still *delivers*. A second run (`hb2`) settled that — messages were published only after the socket had already been open for an hour:

```
03:05:00 [hb2] alive 1h0m0s · msgs=0 · outstanding_pings=0 · last_rtt=9ms
03:05:09 published 3 messages through the tunnel
         journal: 3 entries, log_seq 1..3

reconnects logged by hb2: 0
```

Delivered **nine seconds after the one-hour mark, over the same socket** — zero reconnects across the entire run. The connection was not merely surviving idle, it was still doing its job at the far end of the hour.

(The first attempt at this missed its trigger: the client's duration format changes to `1h10m0s` past an hour and the watcher was matching `70m`. The socket was fine; the observer was wrong.)

### Without it — dead at 2 m 05 s, every single time

```
[nohb] DONE total=1h20m0s reconnects=35 longest_single_socket=2m5s

socket lifetimes:
     35 x 2m5s
      1 x 25s        <- final socket, cut short by the run deadline
```

**Thirty-five sockets, every one lasting 2 m 05 s.** Not "about two minutes" — the same value 35 times. That is an edge timer, not a flaky network, and it is the clearest possible confirmation of the ~100 s idle timeout IDEA-24 names (125 s observed, consistent with a ~100 s idle window plus close detection).

The control had to ask the server to stop pinging too (`?silent=1`). With the server heartbeating, even a client that never pings has frames arriving every 35 s, so the socket is not idle and the control would have "passed" for reasons having nothing to do with the client. That is worth stating because it is an easy way to accidentally prove nothing.

## 2. Forced kill — zero loss, zero duplication

`killtest.sh`, run end to end through the tunnel. `kill -9`, not a graceful stop: a clean shutdown lets the client flush state it will not have when a phone goes into a tunnel.

```
phase 1: 5 messages, client running          journal 5,  cursor 5
phase 2: client running, kill -9 mid-stream  journal 10, cursor 10
phase 3: 10 more messages while it is dead   server head 20
phase 4: restart, resume from cursor         journal 20, cursor 20
phase 5: republish the same idempotency keys server head 20   <- unchanged

verify: client journal vs server log
  server messages : 20
  client journal  : 20 lines, 20 distinct
  DUPLICATED      : 0
  LOST            : 0
  PHANTOM         : 0
  journal in seq order: True

RESULT: PASS — zero loss, zero duplication
```

Three properties, each deliberate:

- **Messages are published while the client is dead**, so there is something to lose. A restart test with a quiet server proves nothing.
- **The journal is fsynced before the message is counted.** Written after the fact it would under-report on a hard kill, which is the one moment it exists for.
- **Phase 5 replays the same `client_id` values.** The server replays the original ack and creates no second message — head stays at 20. A retry after an ambiguous failure is free, which is what an offline outbox is built on.

> Resume-from-cursor is what makes reconnection **a query rather than a guess**. The client knows exactly what it has, so a duplicate delivery is *detectable* rather than a design flaw.

## 3. The silent failure, reproduced deliberately

The dangerous case IDEA-24 names is not a socket that closes — it is a mobile network dropping TCP without a FIN, leaving **both ends believing the socket is open**. The client shows no error, receives nothing, and looks exactly like a quiet conversation.

You cannot reach that state by killing a process; that is a clean close the client notices at once. The server has to **go deaf while staying connected**, which is what `?deaf_after=<sec>` on the rig does.

Run with a 10 s heartbeat, limit 2, server going deaf at t=15 s:

```
01:21:24 alive 20s · outstanding_pings=0 · last_rtt=16ms    <- deaf at t=15s
01:21:34 alive 30s · outstanding_pings=1
01:21:44 alive 40s · outstanding_pings=2
01:21:44 socket closed: severed: 2 unanswered pings (limit 2)
01:21:46 ready · resumed=true                               <- healthy again
01:21:56 alive 10s · outstanding_pings=0 · last_rtt=8ms
```

The client severed **itself**, deliberately, rather than waiting for the OS to notice — and resumed from its cursor two seconds later.

### The number this puts a name to

Detection latency ≈ `heartbeat × (missed_pong_limit + 1)`. Measured: 10 s × 2.5 = 25 s from deaf to severed.

**At the production 35 s heartbeat that is 70–105 s** of a client believing it is connected when it is not. That is comfortably under Cloudflare's ~100 s idle window, which is what the interval was chosen for — but it is the real cost of the interval and worth stating rather than discovering. A longer interval is cheaper on battery and radio and proportionally slower to notice a dead socket.

**It is a dial, not a constant.** Per R4 it ships in the `ready` frame (`heartbeat_interval_sec`, `missed_pong_limit`), so it can be turned server-side without shipping a client — and it cannot drift between the Vue and Flutter implementations, because neither one owns it.

## 4. What was actually under test

The rig speaks the **R4 wire protocol**, not an ad-hoc test format: every frame is a generated type from `schema/catenary.wire.v1.schema.json`. That matters — a rig with its own private protocol can prove a transport nobody is going to build.

Closing the loop the other way, a **real `/sync` response from the Go rig decodes cleanly through the generated TypeScript client** (`captured-sync-response.json`, checked by `verify.sh`). The server, the schema and the web client agree on the wire, and that is now a regression test.

The rig is an append-only log with a monotonic ordinal and a sync protocol — the design IDEA-23 describes — and not a chat app. It is ~400 lines, in memory, and disposable.

### Safety of the exposure

The rig binds **loopback only** and was reached through an **ephemeral `trycloudflare.com` quick tunnel**, so no production tunnel or DNS was touched. Every endpoint requires a random 30-character bearer token, verified: an unauthenticated `/publish` returns **401**. It serves no real data. Tokens and tunnel URLs are gitignored and are dead the moment the rig stops.

Caveat worth naming: a **quick tunnel is not the production named tunnel**. Same Cloudflare edge and the same idle-timeout behaviour — which is the risk under test — but the deployed path adds Traefik and the split-entrypoint setup. Re-running the idle test once through the real path is cheap and worth doing before P1 depends on it.

## 5. Verdict

R1 passes, and the margin is not narrow: **80 minutes on one socket at 9 ms, against a control that could not hold one for more than 125 seconds.** The heartbeat is doing the work, and the amount of work it is doing is measured rather than assumed.

IDEA-24 says that if this fails, say so and stop — long-polling or SSE is a different architecture and adopting it quietly is how the project ends up worse than Discord at the thing it exists to be better at. **It did not fail.** The WebSocket architecture stands.

```
spike/r1-websocket/
  killtest.sh                   forced-kill test, end to end
  killtest/                     its journals, logs and verification output
  deaftest/RESULT.md            missed-pong severance, with fault injection
  captured-sync-response.json   real rig output, validated against the schema
  hb.journal / nohb.journal     the two 80-minute clients
server/cmd/r1rig/               the server (~400 lines, in memory)
server/cmd/r1client/            the client, with its heartbeat and outbox journal
```
