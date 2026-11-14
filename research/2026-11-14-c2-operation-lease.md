# 2026-11-14: C2, an operation lease on the destination cluster

**Question:** does an operation lease (design C2) stop two hubs from running
the same sync, and how much of the baseline churn does it remove on its own?

**Short answer:**
- It does serialize operations. When both hubs discover a new commit in the
  same second, exactly one hub syncs (E7: 1 sync, versus 2 with the lease off).
- It does **not** stop revision-skew flapping (E2: 9 syncs, versus 11 on
  stock). The hubs now take turns instead of overlapping, but hub-b still
  reverts to its old commit each time it gets the lease. That needs C1, the
  revision pin, as predicted in the baseline note.

## What was built

Commit [`aeb859186`](https://github.com/ImIOImI/argo-cd/commit/aeb859186)
(+ gofmt fix [`30f2ed365`](https://github.com/ImIOImI/argo-cd/commit/30f2ed365)):

- [`controller/multihub/lease.go`](https://github.com/ImIOImI/argo-cd/blob/30f2ed365/controller/multihub/lease.go):
  one `coordination.k8s.io/v1` **Lease per Application on the destination
  cluster** (namespace `argocd-coordination`, created on demand). It's named
  from `installationID` + the Application instance name, the same identity
  resource tracking uses. Updates use resourceVersion compare-and-swap.
  A lease that isn't renewed within 60s expires and can be taken over.
  `leaseTransitions` is the fencing token. A 10s **settle window** delays
  *auto*-sync after another hub releases, so a comparison made while that
  hub's sync was still running gets redone first. User-initiated operations
  don't wait for it.
- [`controller/appcontroller_multihub.go`](https://github.com/ImIOImI/argo-cd/blob/30f2ed365/controller/appcontroller_multihub.go):
  the glue code. Destination clients come from the controller's existing
  cluster credentials.
- `controller/appcontroller.go`, three call sites:
  - `autoSync` acquires the lease before writing the operation. If another
    hub holds it, it skips and requests a refresh after ≤15s.
  - `processRequestedAppOperation` renews the lease on every pass. If another
    hub holds it, the operation waits (re-checked every 5s). An operation
    already in progress whose lease was taken over is stopped with an error,
    so a hub never acts on a stale lease.
  - The lease is released when the operation completes.
- Off unless `ARGOCD_MULTIHUB_HUB_ID` is set. Unit tests: 10 in
  `controller/multihub` (acquire, renew, deny, expiry takeover with a new
  generation, settle window, release by a non-holder, lease naming). The
  existing `./controller/` suite passes unchanged (92s).

## Setup

The same lab as the [baseline](2026-11-14-baseline-stock-argo.md). Both hubs
run the fork's `argocd` binary layered onto the v3.6.1 image (every Argo CD
component), with the fork's CRDs applied. hub-a/hub-b reconcile intervals are
still 30s/180s.

**Caveat:** this is the fork's binary (master, ~v3.7-dev) on v3.6.1 images,
so its numbers are not strictly comparable to the stock v3.6.1 baseline.
E7 has a proper control: the same binary with multihub off.

## How it was run

Scripts are in `hack/multi-hub-lab/`, run from that directory. Same
environment as the baseline, plus Go 1.27.0 (fetched automatically through
`GOTOOLCHAIN=auto`).

| Step | Command | Script (pinned) |
|---|---|---|
| Unit tests | `go test ./controller/ ./controller/multihub/ -count=1` (repo root) | code @ [`30f2ed365`](https://github.com/ImIOImI/argo-cd/tree/30f2ed365/controller/multihub) |
| Deploy fork, multihub **on** (first attempt failed: `manifests/crds` needs `-k`; fixed in 188f7a822) | `./deploy-fork.sh` → image `argocd-multihub:188f7a822` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) |
| Re-enable auto-sync + self-heal on both | `./selfheal.sh on` | [`selfheal.sh` @ ded013cef](https://github.com/ImIOImI/argo-cd/blob/ded013cef/hack/multi-hub-lab/selfheal.sh) |
| Wait for both hubs to sync | `./wait-synced.sh` | [`wait-synced.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/wait-synced.sh) |
| **E2** (skew), multihub on | `./bump.sh` → `90d6c6a`, 13:07:52Z | [`bump.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/bump.sh) |
| **E6** (wipe), multihub on | `./wipe-app.sh` at 13:15:35Z | [`wipe-app.sh` @ 07e28cdae](https://github.com/ImIOImI/argo-cd/blob/07e28cdae/hack/multi-hub-lab/wipe-app.sh) |
| **E7** (simultaneous discovery), multihub on | `./bump.sh && ./refresh-both.sh` → `314d2df`, 13:19:08Z | [`refresh-both.sh` @ 36e7e2c79](https://github.com/ImIOImI/argo-cd/blob/36e7e2c79/hack/multi-hub-lab/refresh-both.sh) |
| Redeploy, multihub **off** (same Go code) | `MULTIHUB=off ./deploy-fork.sh` → image `argocd-multihub:314d2dfb1` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) (unchanged since) |
| **E7 control**, multihub off (attempt 1 invalid, see below) | `./bump.sh && ./refresh-both.sh` → `9f433aa` 13:25:08Z; rerun → `659a0c4` 13:27:45Z | as above |
| Sync timelines | `AFTER=<t> ./syncs.sh` | [`syncs.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/syncs.sh) |
| Lease / auto-sync decisions | `AFTER=<t> [ALL=1] ./decisions.sh` | [`decisions.sh` @ 90d5d5dc3](https://github.com/ImIOImI/argo-cd/blob/90d5d5dc3/hack/multi-hub-lab/decisions.sh) |
| Save raw data | `SINCE=<w> ./collect.sh ../../research/data/2026-11-14-c2-operation-lease/<exp>` | [`collect.sh` @ 07e28cdae](https://github.com/ImIOImI/argo-cd/blob/07e28cdae/hack/multi-hub-lab/collect.sh) |

**Provenance:** `decisions.sh` was first run as a one-off command and saved
afterwards (90d5d5dc3). It was re-run on the E7 control window and reproduced
the same output. The E2/E6/E7-on decision logs quoted below come from the
saved data (`*-controller-autosync.jsonl`), because the redeploy for the
control run replaced those controller pods. All other scripts were committed
before they ran.

Raw data: [data/2026-11-14-c2-operation-lease](data/2026-11-14-c2-operation-lease)
(`e2/`, `e6/`, `e7-multihub-on/`, `e7-multihub-off/`, each with
`spoke-leases.yaml`).

---

## R1 (E2). Revision skew with self-heal on: serialized, still flapping

`AFTER=2026-11-14T13:07 ./syncs.sh`:
```
== hub-a
13:08:02Z rev=90d6c6a selfHealAttempt=0
13:09:08Z rev=90d6c6a selfHealAttempt=1
13:09:37Z rev=90d6c6a selfHealAttempt=2
13:10:01Z rev=90d6c6a selfHealAttempt=3
13:10:21Z rev=90d6c6a selfHealAttempt=4
== hub-b
13:08:57Z rev=188f7a8 selfHealAttempt=1
13:09:18Z rev=188f7a8 selfHealAttempt=2
13:09:47Z rev=188f7a8 selfHealAttempt=3
13:10:11Z rev=188f7a8 selfHealAttempt=4
```

- **9 syncs** for 1 commit, compared with 11 on stock. The lease handed off
  9 times (`leaseTransitions: 9`), **strictly alternating** a→b→a→…, and no
  two operations overlapped.
- Lease denials on both sides, with reasons:
  `settling after an operation by hub-b`, `operation lease held by hub-b`,
  and the hub-a equivalents. The settle window added about 10s between
  turns.
- The flapping ran from 13:08:57 to 13:10:21Z (~1.5 min). The spoke ended on
  generation 4.
- **As predicted:** the lease decides *who* acts, not *what* they apply.
  Hub-b's comparison still pointed at its old commit (`188f7a8`), so each
  turn it got was a revert. **C1 (revision pin) is required.**

## R2 (E6). Wiping the app on the spoke: confounded

`./wipe-app.sh` deleted the `lab` namespace. Results (`e6/`):

- hub-a took the lease at 13:15:34Z, *before the namespace deletion had
  finished*, and synced at 13:15:35Z into a namespace that was being deleted.
- hub-b was denied (`operation lease held by hub-a`) twice, then waited out
  the settle window twice, then took the lease (generation 11) and synced at
  13:15:55Z.
- hub-b's sync was **legitimate**, not a duplicate: hub-a's work went away
  with the namespace.
- Both were self-heal syncs, which skip hooks (baseline R2), so this test
  says nothing about hooks either.

**Conclusion:** the lease behaved correctly (deny → settle → acquire), but
E6 doesn't test duplicate syncs. It's kept here because the design flaw it
exposed is worth noting: **a "wipe" test needs to wait for namespace
deletion to finish before either hub can react, which a lab can't
guarantee.** Use E7 instead.

## R3 (E7). Both hubs discover a new commit in the same second

`./bump.sh && ./refresh-both.sh`: both hubs hard-refresh together and both
want a *full* sync (PreSync hook included) of the same new revision. This
is the R0 situation from the baseline, on demand.

| | multihub **on** (`314d2df`) | multihub **off**, same binary (`659a0c4`) |
|---|---|---|
| Full syncs started | **1** (hub-a 13:19:10Z) | **2** (hub-a *and* hub-b, both 13:27:46Z) |
| Other hub | denied ×4 in the same second (`operation lease held by hub-a`), then `Synced` | started its own sync |
| PreSync hook Jobs | 1 | 1 (see below) |
| Lease | generation 12, released after the sync | none |

- **The lease works where it matters:** with it on, exactly one hub ran the
  operation.
- **Why the control had only one hook Job:** both hubs started in the same
  second, so both computed the same hook name
  `lab-presync-659a0c4-presync-1794662866`
  (`<generateName><sha7>-<phase>-<startedAt unix>`, baseline F5). The
  second create ran into the first hub's Job. That's luck: in the baseline R0
  the two syncs started 19s apart and **two** hook Jobs ran. Without the
  lease, whether a hook runs twice depends on the clock. With the lease, it
  can't.
- **Control attempt 1 was invalid** (`9f433aa`, 13:25:08Z, data folded into
  `e7-multihub-off/`). The redeploy had restarted hub-b's repo-server, and
  its hard refresh spent **73s** re-cloning the fork (13:25:08 → 13:26:21Z),
  so hub-b never raced. Rerun once both caches were warm.

---

## Scoreboard changes

| Problem | Stock v3.6.1 | Fork, C2 only |
|---|---|---|
| Syncs per commit, self-heal on (E2) | 11 (overlapping) | 9 (serialized) |
| Full syncs when both hubs discover a commit together (E7) | 2 (control, same binary) | **1** |
| Hook runs in that case | 1–2, depending on whether start times share a second | **1** |

## Next

- **C1, the revision pin.** This is the change that should take E2 from 9
  syncs to 1.
- Not tested yet:
  - lease **takeover after expiry** (kill hub-a's controller mid-sync);
  - the **in-progress operation stops when its lease was taken over** path;
  - an **operation started by hand** waiting behind another hub's lease.
  All three are covered by unit tests only.
- Known gap: operations that fail before reaching the final
  `setOperationState` (e.g. a retry-schedule error) don't release the lease.
  It expires after 60s instead.

**Update (later the same day):** the third item (an operation started by hand waiting behind another hub's lease) is now verified live. See [C2 telemetry, R1](2026-11-14-c2-telemetry.md#r1-v1-defaults). The logging gaps raised after this note are addressed there too.
