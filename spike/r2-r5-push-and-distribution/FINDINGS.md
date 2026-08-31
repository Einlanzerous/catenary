# R2 + R5 — push, and getting the app onto a friend's phone

**Gates:** IDEA-25 (R2) and IDEA-28 (R5) · **Status: NOT CLEARED** · **Researched:** 2026-08-17

> Both tickets say these trade directly against each other and should be resolved together. They now resolve together much harder than either ticket anticipated, because the ground moved.

## What this is, and what it is not

**Neither gate is cleared, and neither can be from this machine.**

- **R2** requires a *cold, Doze-sleeping* Android device receiving notifications *within seconds*, *consistently*, over a *multi-hour* test. That needs a physical phone, a Firebase project, and hours of wall-clock. There is no Android device attached to this box.
- **R5** requires *a friend installing it without a phone call*. It is deliberately behavioural and is measured on someone who did not build it. It cannot be self-tested, by construction.

What *is* done: the decision both tickets said to make jointly, on current evidence, plus a test protocol precise enough to run in one sitting when a phone and a willing friend exist. **Treat the recommendation as a decision to ratify, not a gate that has been passed.**

---

## The thing that changed: Android developer verification

Both tickets frame R5 as *"signed APK sideload (no Google involvement) vs Play internal testing (a Google dependency)"*. **That framing is now obsolete**, and the timing is uncomfortably tight — the relevant changes land **this month**.

Google is rolling out **Android Developer Verification**: apps must be registered to an identity-verified developer to install on a *certified* Android device — **regardless of source**, Play Store or sideloaded APK alike.

| when | what |
|---|---|
| April 2026 | `Android Developer Verifier` system service appears on devices |
| **August 2026** | **Advanced Flow ships, and free limited-distribution accounts open** |
| September 2026 | Enforcement begins — Brazil, Indonesia, Singapore, Thailand |
| 2027 | Global rollout to all certified Android devices |

### What sideloading an unverified app now costs the recipient

The "Advanced Flow" is the escape hatch for apps from unverified developers. In full, the recipient must:

1. enable developer mode in system settings,
2. **confirm they are not being coached**,
3. **restart the phone** and re-authenticate,
4. **wait 24 hours**,
5. confirm with biometrics or PIN,
6. then install — indefinitely, or for seven days.

And the option is deliberately buried in developer settings so it does not surface to users who are not already looking for it.

> **R5's exit criterion is "a friend installs it without a phone call from you." The Advanced Flow is a phone call, a reboot, and a day of waiting.** Bare sideloading is not a viable primary distribution path. It is not close.

This is not a criticism of the mechanism — the 24-hour cooling-off exists to break scam-coaching, which is a real problem it plausibly addresses. It simply means the option R5 was weighing no longer exists in the form it was weighed in.

### The free tier is the genuinely new option

Google is opening a **free limited-distribution developer account**:

- **unlimited apps to up to 20 devices**
- **no government ID**
- **no $25 fee**

Twenty devices is a family group. This is the closest thing to what the ticket originally imagined by "signed APK sideload", and it did not exist when the ticket was written.

Two caveats before leaning on it: the cap is on **devices, not people** — a friend replacing a phone plausibly consumes another slot — and **it does not solve updates**. A sideloaded app has no update channel unless one is built, and the tickets are right that this is the underestimated half: *"a build that can be installed once but never updated turns every future fix into six phone calls."*

---

## R5 — recommendation: **Play internal testing**

The ticket's cost/benefit inverts once verification is accounted for.

| | Sideload (free verified account) | **Play internal testing** |
|---|---|---|
| Google dependency | **Yes** — verification is required from 2027 regardless | Yes |
| Identity check | None (free tier) | Government ID |
| Cost | Free | $25, one-off |
| Recipients | 20 **devices** | 100 **testers**, by email |
| Install flow | Trust an APK from a message | Familiar Play install |
| **Updates** | **Build it yourself** | **Free and automatic** |
| Review | None | **None** — internal testing skips review |
| 12-testers/14-days rule | n/a | **Does not apply** |

Two points that decide it:

**1. The Google dependency is no longer avoidable, so it is no longer a differentiator.** From 2027 globally, distributing an Android app to non-technical people requires Google-verified developer identity whichever path you take. Sideloading's entire premise — "no Google involvement" — expires. Once you are paying that cost anyway, Play internal testing gives strictly more for it: a real update channel, a familiar install flow, and 100 testers instead of 20 devices. The marginal cost over the free tier is $25 and an ID check.

**2. The 12-testers-for-14-days rule does not apply.** Worth stating plainly because it is the most common misconception about Play in 2026 and would look like a blocker: that rule gates **production** access and lives on the **closed testing** track. **Internal testing** is up to 100 testers, **no review**, instant distribution, and never touches it. Catenary has no reason to ever publish to production, so the rule is simply not on the path.

**Keep the free limited-distribution account as the fallback**, registered early. It is free, and it is the answer if the ID check is unacceptable or if Play ever becomes a problem. Registering costs nothing and preserves the option.

---

## R2 — recommendation: **FCM**, and UnifiedPush's case has collapsed

### The ticket's key assumption is confirmed

> *"No Play review is required for private distribution — this is worth confirming early, because assuming otherwise is what pushes people to the harder option for no reason."*

**Confirmed, from Firebase's own documentation:**

> *"FCM clients require devices running Android 6.0 or higher that also have the Google Play Store app installed… Note that you are not limited to deploying your Android apps through Google Play Store."*

The device needs Google Play services. **The app does not need to be published, listed, or reviewed.** (Note that older third-party guides claiming you must upload to the Play Console to use FCM are wrong and out of date — this tripped up the first pass of this research and is worth not re-deriving.)

### Why UnifiedPush + ntfy no longer competes

Both tickets already note the install friction — a second app plus a server URL — fails R5's criterion on its own. That was always true. What is new:

> **UnifiedPush's entire value proposition was avoiding a Google dependency, and the distribution path is now Google-verified regardless.**

You would pay the friction cost and still not be de-Googled: the phone is a certified Android device, the app is registered to a verified developer, and Play services is present anyway. That is the worst of both.

UnifiedPush stays the right answer for a **de-Googled-device** audience (GrapheneOS, LineageOS, /e/OS). Catenary's audience is "a small trusted group" on the phones they already carry, which is the stock-Android case. If that assumption is wrong for this specific group, this conclusion flips — so it is worth checking rather than assuming.

### Design note that survives either choice

The payload carries **no message content** — a notification is a wake signal, and the client fetches through the sync path. This keeps one code path for message delivery and keeps message text out of a third party's infrastructure, which matters *more* given D1 declines E2EE. The R4 wire schema already supports this: a wake signal maps onto the existing `/sync?after=<log_seq>` cursor with no new frame type.

---

## The test protocol, for when hardware exists

R2 is the gate that decides whether the project is worth building, and it stays open. Its exit criterion is precise, and every word is load-bearing. Running it should take one evening.

**Setup**
1. Firebase project; `google-services.json` in the APK; service-account key held server-side.
2. Server sends a **data-only** message (not `notification`) with `priority: high`, payload carrying `conversation_id` + `log_seq` only.
3. Client wakes, calls `/sync?after=<cursor>`, posts the notification locally.

**Measurement — record latencies, not pass/fail.** *"'Within seconds' that is sometimes 4 minutes is a different product."* Log server send time and client-receipt time; report the distribution, especially the tail.

**Conditions, in increasing order of how much they hurt**
1. Screen on, app foregrounded — the case that always works. Baseline only.
2. App backgrounded, screen off.
3. **App force-stopped / cold** — not in memory, not recently foregrounded. Note that a user-initiated force-stop suppresses FCM until the app is next opened; that is expected Android behaviour and must not be confused with a delivery failure.
4. **Doze, forced and confirmed** — `adb shell dumpsys deviceidle force-idle`, and verify the state rather than assuming it.
5. **Multi-hour, overnight.** Doze buckets tighten the longer a device sits, so a 20-minute test measures the easy case. Send on a randomised interval across 6+ hours and collect every latency.

**Hardware.** Vendor battery optimizers (Samsung, Xiaomi) are aggressive well beyond stock Doze. **Test on the phones the group actually carries**, not on a Pixel — a Pixel result does not generalise to a Samsung, and the group's phones are the only ones that matter.

**Expected failure mode to watch for:** high-priority data messages are supposed to punch through Doze. If they do not, the fallback is a `notification` message (displayed by the system, less control) or accepting a delay. If neither is acceptable, **that is R2 failing, and IDEA-23 is explicit that the honest outcome is to close the spike rather than descope around the gate.**

---

## Summary

| | recommendation | confidence | status |
|---|---|---|---|
| **R5** | Play internal testing; register the free limited-distribution account as a fallback | High — the verification change largely forces it | **Not cleared** — needs a real friend |
| **R2** | FCM, data-only payload carrying no content | High — its main competitor lost its rationale | **Not cleared** — needs a phone and a multi-hour test |

The decision half of both tickets is done and points the same way. The empirical half of both is untouched, and R2's remains one of the two gates that decide whether Catenary is worth building at all.

## Sources

- [Understanding Android developer verification — Android Developer Console Help](https://support.google.com/android-developer-console/answer/16561738?hl=en)
- [This is Android's new 'advanced flow' for sideloading apps without verification — 9to5Google](https://9to5google.com/2026/03/19/android-advanced-flow-sideloading/)
- [Google Adds 24-Hour Wait for Unverified App Sideloading — The Hacker News](https://thehackernews.com/2026/03/google-adds-24-hour-wait-for-unverified.html)
- [Google sets timeline for Android developer verification enforcement — Help Net Security](https://www.helpnetsecurity.com/2026/06/19/android-developer-verification-rollout-markets/)
- [Android's new developer verification rollout begins — Android Authority](https://www.androidauthority.com/android-developer-verification-rollout-sideloading-flow-3653395/)
- [Get started with Firebase Cloud Messaging in Android apps — Firebase docs](https://firebase.google.com/docs/cloud-messaging/android/client)
- [Internal vs Closed vs Open Testing on Google Play (2026 Guide)](https://www.testerscommunity.com/guides/internal-vs-closed-vs-open-testing-google-play)
