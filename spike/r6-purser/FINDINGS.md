# R6 — does Purser's connector contract generalize to Catenary?

**Gate:** IDEA-29 · **Status:** cleared — the abstraction holds · **Measured:** 2026-08-17

> *Exit criterion: the stub provisions a `catenary` account. The deliverable is the finding, not the code — either the abstraction holds, or the specific place it pinches is written down as a Purser ticket.*

**It holds.** `connector.Connector` accepts Catenary with no widening, no extra method, and no optional side-channel. The compile-time assertion is in the stub and it is a real part of the answer:

```go
var _ connector.Connector = (*Connector)(nil)
```

```
go test ./...   ok  github.com/Einlanzerous/purser/spike-r6/catenary
```

Seven tests, one per question the gate asked plus the hazards they turned up.

---

## Where the stub lives, and why that is itself a finding

Purser's contract is in `internal/connector`. Go's internal rule means **only a package whose import path is inside `github.com/Einlanzerous/purser/...` can implement it** — so a connector cannot live in the service's own repo. It has to be Purser's.

That is the right design (connectors are Purser's model of other systems, not those systems' model of themselves) and worth stating, because "Catenary ships its own Purser connector" is a plausible-sounding arrangement that the module boundary quietly forbids.

The stub takes the module path `github.com/Einlanzerous/purser/spike-r6` with a `replace` at the local checkout, so it compiles against the **real** interface while its files stay in the Catenary tree. **Nothing was written into the Purser repo.**

---

## Q1 — Provision returning per-device credentials

> *The existing connectors mint one secret per person per service; Catenary's unit is a device. Does the contract express that, or does it want one secret and get a list?*

**It wants one secret and gets one secret.** The mismatch is real in Catenary's data model and never reaches the connector boundary — because of *when* things happen.

At provisioning time the person has **zero devices**. They have not installed anything. The only credential that can exist is a bootstrap **enrolment token**, which is exactly one string. Per-device refresh tokens are minted by Catenary when a device redeems that token, long after Purser is out of the picture.

> **Purser provisions people. Catenary tracks devices. They never have to agree on a unit because they never exchange one.**

`Result.Secret` fits without strain and `Result.Extra` is not needed — the test asserts `Extra` is empty, because needing it would have been the first sign of the abstraction bending.

One difference from Lyceum worth carrying into P1: Lyceum's `409 → success with no secret` branch would leave a **re-invited** person with nothing redeemable, since a single-use token that has already been redeemed cannot be handed out again. Catenary's Provision re-issues instead. That is a *rotation*, which Provision is permitted to do and Reconcile explicitly is not.

## Q2 — What does Reconcile compare against?

> *Purser records secrets only as a sha256 hash, and reconcile is read-only by contract. What does it compare against when the upstream truth is "this person has three registered devices, one revoked"?*

**Nothing, and it should not try.** `ReconcileResult` carries identity only, and the question Purser is asking is *"does this person have access to this service"* — an **account-level** fact. Device counts are Catenary's operational detail and Purser has no column for them; a connector that packed `"3 devices, 1 revoked"` into `Username` to make them visible would be inventing a schema inside a string field.

The real trap is subtler, and the stub gets it wrong in an earlier draft the way anyone would:

**`Exists` must key on account STATUS, not on the row existing.**

| upstream state | `Exists` | why |
|---|---|---|
| active account, every device revoked | **true** | they can enrol a new device — they have access |
| disabled account, devices un-revoked | **false** | however many device rows remain, they cannot get in |
| no account | false | — |

`Exists: row != nil` gets the second row wrong, and an audit would then leave a disabled account looking healthy.

## Q3 — Deprovision's fan-out

> *Revoking a person means revoking every device's refresh token. That is a fan-out the other three connectors do not have.*

The fan-out is fine — it is internal to the connector, and the contract never promised a single upstream call. What it introduces is something the other three genuinely do not have:

> **Deprovision is no longer atomic upstream, so it can half-succeed.**

And that interacts with Reconcile in a way that makes the **order load-bearing**. Two tests differ only in ordering, with the same injected failure on the second device:

**Devices first (wrong).** Revoke device 1, fail on device 2, return an error. Left behind: an **active** account, partly revoked. Reconcile reports `Exists: true` — and that is not even a wrong answer, because the person really can still enrol a new device. The offboard **failed open**, and nothing in Purser's model can tell the difference between that and an offboard that never started. On the one command you cannot take back.

**Disable first (correct).** Disable the account, then revoke. A failure partway leaves a **disabled** account and some un-revoked device rows. Access is genuinely gone — a disabled account cannot refresh a token or enrol a device — Reconcile truthfully reports `Exists: false`, and the retry mops up the stragglers. Idempotent, and safe to leave half-done.

This is the one thing here you could not have got from a design review, and it is the reason the gate asked for a stub.

---

## What this means for Purser

**No Purser ticket is required.** The contract generalizes; four connectors are not four special cases sharing a signature.

Two things are worth recording somewhere durable, because both are invariants a future connector could violate without any compile error:

1. **`Reconcile.Exists` means "can get in", not "has a row upstream."** Every existing connector happens to have these coincide. Catenary is the first where they do not, and the doc comment on `ReconcileResult` could say so directly — it currently says "whether the person currently has access upstream", which is right but reads as a restatement rather than a warning.
2. **A connector whose Deprovision fans out must revoke access before cleaning up detail.** This is a new obligation that the interface cannot express and that no existing connector needed. It belongs in `Deprovision`'s doc comment next to the existing idempotency requirement.

Neither is a change to the interface. Both are notes on it — which is a much better outcome than the gate was braced for.

---

## Caveat on what was actually proved

The upstream here is `fakecatenary`, an in-memory stand-in, because **Catenary's admin API does not exist yet**. So this validates the *contract fit* and the *ordering hazard* — which is what R6 asked about — and not any real integration. When the real API lands, the connector moves into `purser/internal/connectors/catenary/` and the fake becomes its test double, which is the same arrangement the other four already use.

```
spike/r6-purser/
  go.mod                       module path under Purser's, replace → local checkout
  catenary/catenary.go         the stub connector
  catenary/catenary_test.go    7 tests, one per question and hazard
  fakecatenary/fake.go         in-memory upstream with account+device model
                               and injectable revoke failure
```
