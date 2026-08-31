# R2 — push to a cold, Doze-sleeping Android device

**Gate:** IDEA-25 · **Status:** cleared · **Measured:** 2026-08-17

> *Exit criterion: a cold, Doze-sleeping device receives a notification within seconds, consistently, over a multi-hour test. Record the delivery latencies, not just pass/fail — "within seconds" that is sometimes 4 minutes is a different product.*

**60 sends across two vendors over ~6 hours. Zero misses. Worst case 6.07 s; p90 at or under 1.31 s on both.**

| device | delivered | min | p50 | p90 | max |
|---|---|---|---|---|---|
| **Pixel 9 Pro** (stock Android 16) | **31/31** | 0.24 s | 0.86 s | 1.21 s | **6.07 s** |
| **Samsung SM-X230** (One UI, Android 16) | **29/29** | 0.15 s | 0.64 s | 0.92 s | **1.48 s** |

Per phase:

| phase | Pixel | Samsung |
|---|---|---|
| cold (process evicted) | 3/3, max 1.18 s | 3/3, max 0.52 s |
| doze (forced, state verified `IDLE`) | 3/3, max 0.71 s | 3/3, max 0.64 s |
| **soak (multi-hour, natural Doze)** | **21/21**, max 6.07 s | **17/17**, max 1.48 s |

Raw per-send data: `results.csv`, `results-tablet.csv`.

---

## 1. Why these numbers mean something

Each word of the criterion was set up deliberately and **verified rather than assumed**:

- **Cold** — `am kill`, *not* `am force-stop`. A user-initiated force-stop suppresses FCM until the app is next opened; that is correct Android behaviour, and testing it would have manufactured a false negative. `am kill` evicts the process the way memory pressure does.
- **Doze-sleeping** — `dumpsys deviceidle get deep` is checked for `IDLE` before the phase runs, and the runner **refuses to run the Doze phase on a charging device**, because Doze never engages while plugged in.
- **Multi-hour** — 5–6 h per device, randomised **10–25 minute** gaps. Long and random on purpose: Doze buckets tighten the longer a device sits undisturbed, and a fixed short interval keeps waking it and measures the easy case forever.
- **Latencies, not pass/fail** — every send is a row; the report is a distribution with its tail.

Both devices **stayed unplugged for the entire run** (Pixel 83% → 60%). At the end the Pixel was in genuine deep Doze (`IDLE`) with the probe decayed to **standby bucket 40 (RARE)** — the app had aged out of every favourable bucket and still received. That is the condition the gate was actually asking about.

**Measurement design.** The server times a round trip: T0 when it hands the message to FCM, T1 when the device's callback arrives. `T1 - T0` is measured entirely on one clock, so device clock skew cannot contaminate it, and it errs in the safe direction — it *includes* the callback's own network hop, so true delivery latency is somewhat lower than reported. The obvious alternative (put `sent_at` in the payload, let the phone subtract) would have measured clock skew as much as latency, and at a "within seconds" threshold a 2 s skew would have swamped the result invisibly.

**Payload:** data-only, `android.priority: HIGH`, carrying only an id and a timestamp. A `notification` message is displayed by the system and may never invoke the app's handler; data-only is the shape the real client needs, because a notification is a wake signal and the client then fetches through the sync path. It is also the only shape whose wake behaviour this test can measure.

## 2. The tail

The Pixel's **6.07 s** outlier is the single most important number here, because it is the one the criterion is really about. Context from the CSV:

```
soak-598853682,soak,2026-08-17T21:46:24Z,true,6069,background,1
                                              ^^^^          ^ callback took 1 ms
```

The callback was instant, so all 6.07 s is genuine delivery latency — not a slow report. It is the only send above 1.67 s on that device: 20 of 21 soak deliveries landed under 1.4 s.

**6 seconds is still "within seconds", and it is nowhere near the 4-minute failure the ticket warns about.** But it is 6× the median, and it is worth stating plainly rather than hiding behind a p50: on stock Android, an occasional multi-second delay under deep Doze is normal and should be expected. For a chat app that is fine. It would not be fine for anything claiming real-time.

Notably the **Samsung was tighter than the Pixel** at every percentile, with a 1.48 s worst case. The vendor-optimiser concern that motivated testing it did not materialise on this hardware.

## 3. Three instrument bugs, and why they are the real lesson

This gate took far longer than the measurement warranted, entirely because of faults in the harness rather than the system. All three made a **working system look broken**:

1. **Silent callback failure.** The probe reported delivery *only* by calling the collector, and swallowed the error if that call failed. The tablet could not resolve the quick-tunnel hostname (`Failed host lookup … errno = 7`), so every callback died quietly — making "FCM never delivered" and "FCM delivered but the callback failed" indistinguishable. Produced a confident **0/9** that measured nothing.
2. **A diagnostic that bypassed the instrument.** `fcmprobe`, built to investigate (1), sends *directly to FCM* without registering the send in the collector's bookkeeping. The app received every message and called back with `probe_id=fcmprobe-…`; the collector looked it up, found nothing, recorded nothing. Every reading after the DNS fix came from a tool talking past the thing being read. Produced a second confident zero, on a device that was working perfectly.
3. **Vendor log suppression.** Samsung suppresses app logcat output, so the logging added to fix (1) was blind on precisely the device under investigation.

The tell was visible the whole time and got walked past: the probe's own UI read **"foreground messages: 5"**.

> Treat "the measurement says zero" as a claim about the instrument until proven otherwise. A silent zero is the same shape as a working system with a broken observer, and this project has now produced that failure three times — the R1 watcher whose regex never matched, and both of the above.

**Fixes carried forward:** the probe logs receipt *before* any network call, retries the callback with backoff, and never swallows an error; the collector exposes `/token` so a registration token is read exactly rather than transcribed off a screenshot (which cost an hour, having produced a spurious `UNREGISTERED`); and the tablet talks to the collector over the LAN by IP, taking DNS and TLS out of the path entirely.

## 4. The on-device oracle worth knowing about

```
adb shell dumpsys activity service com.google.android.gms/.gcm.GcmService
```

gives per-message delivery accounting straight from Play services, independent of any harness:

```
12:49:38.971 net=1: Received dev.catenary.pushprove 0:1786988980078636%...
12:49:40.939 net=1: Acked dev.catenary.pushprove:0 0:1786988980078636%...
12:49:40.960 net=1: Successful broadcast to dev.catenary.pushprove (time=1722ms priority=HIGH)
```

It also shows the live push connection (`connected=mtalk.google.com…port=5228`, `failedLogins=0`) and the adaptive heartbeat state. **Check this first next time** — it is the one source of truth that does not depend on our own code, and it would have collapsed the entire investigation above into about five minutes.

## 5. Verdict and what it means for the build

**R2 clears.** FCM delivers to cold, deep-Doze devices within seconds, consistently, over hours, on both stock Android and Samsung.

- **Use FCM**, data-only, `priority: HIGH`.
- **The payload carries no message content** — a wake signal only, with the client fetching through `/sync?after=<log_seq>`. One code path for message delivery, and message text stays out of Google's infrastructure, which matters more given D1 declines E2EE. The R4 wire schema already supports this with no new frame type.
- **UnifiedPush is not needed and its rationale has collapsed** — see IDEA-28. Its only value was avoiding a Google dependency, and Android's developer verification makes the distribution path Google-verified regardless.

## 6. Caveats

- **Two devices, one network, one Firebase project.** Both were on Wi-Fi throughout; **cellular was never exercised**, and a phone switching between networks is a plausible source of additional latency.
- **No Xiaomi/OnePlus/etc.** The Samsung result is reassuring but does not generalise to every OEM optimiser.
- **~38 soak sends total.** Enough to establish that delivery is reliable and to characterise the tail roughly; not enough to quantify a p99.
- The Samsung tablet dropped off wireless adb mid-soak and its results were unaffected, because the probe reports over the LAN rather than through adb. That is the measurement design working as intended.

```
spike/r2-push/
  app/                   Flutter probe (disposable; not the real client)
  run-phases.sh          background / cold / doze phases, each verified
  results.csv            Pixel 9 Pro, per-send
  results-tablet.csv     Samsung SM-X230, per-send
  secrets/               Firebase key + google-services.json (gitignored)
server/cmd/r2collector/  sender + collector + latency reporting
server/cmd/fcmprobe/     single-send debug tool — see instrument bug (2)
```

---

## Note added at graduation (2026-08-30)

`app/android/app/google-services.json` is **not committed**. It is a live Firebase config for the `catenary-push-probe` project, and this repository is public. Google ships that file inside every APK, so it is not a secret in the strict sense — but it is a real key against a real project, it regenerates from the Firebase console in a minute, and the house rule puts the copy of record in Signet. `google-services.json.example` shows the shape.

Nothing in this directory's measurements depends on rebuilding the probe app. E7 (`CANT-51`) needs the **production** Firebase project's config, not this one's.
