# 2026-11-14: C1, a revision pin shared through the destination cluster

**Question:** if every hub compares and syncs against one agreed revision,
does revision-skew flapping go away?

**Short answer: yes.** With self-heal on and the hubs polling Git at 30s and
180s, one commit now causes **1 sync** (stock v3.6.1: 11; C2 lease only: 9).
- The stale hub **adopted** the newer pinned revision within a second of
  the other hub setting it, and never reverted.
- The C2 lease still stopped it from starting a duplicate sync while the
  first one ran.
- When both hubs discover a commit in the same second (E7), it's still
  **1 sync, 1 hook run**.

First feature built under the TDD rule: four red → green pairs, each proven
with `hack/tdd/red-green.sh`.

## What was built

The pin is stored as annotations on the Application's existing `Lease` on
the destination (`multihub.argoproj.io/pinned-revision`, `-commit-time`,
`-by`, `-at`). It shares the lease's compare-and-swap, so no new object
type is needed.
- **Ordering is by commit time**: the pin moves only to a strictly later
  commit, and ties keep the existing pin. This is a spike approximation of
  "is a descendant of" (see Known gaps).
- **Writes only when needed**: the pin is written only when it's created
  or moves. Agreeing with it or adopting it is a read.
- **Independent of the operation lease**: pinning never touches the lease's
  holder or fencing token.

The controller wiring has four parts:
- After every comparison, `processAppRefreshQueueItem` offers the compared
  revision (with its commit time) to the pin.
- If the pin differs, the hub is behind, so it **compares again at the
  pin**. Status and auto-sync only ever see the agreed revision.
- The commit time comes from the repo-server's revision metadata (cached
  there), the same way the hydrator gets it.
- These apps are skipped and behave like a single hub: multi-source apps,
  helm and OCI sources, and anything where the commit time or pin can't be
  read.

**Telemetry:**
- Every pin decision is logged with `multihub.path=pin` and
  `multihub.event` (`pinned`, `advanced`, `adopted`, `kept`, `error`), plus
  `candidate_revision`, `pinned_revision` and `pinned_by`.
- Each decision is counted in
  `argocd_multihub_lease_decisions_total{path="pin"}`.
- Moves of the pin follow `LOG_LIFECYCLE`. `kept` is logged at debug.
  Errors warn on change.

## TDD record

| Pair | Red | Green | Proof |
|---|---|---|---|
| Pin store (`controller/multihub`) | [`64029492c`](https://github.com/ImIOImI/argo-cd/commit/64029492c): 11 FAIL; 2 invariants pass | [`809e9ecb8`](https://github.com/ImIOImI/argo-cd/commit/809e9ecb8) | `red-green.sh 64029492c f5a5b473c ./controller/multihub/` → PROVEN |
| Controller compares at the pin | [`029c25404`](https://github.com/ImIOImI/argo-cd/commit/029c25404): 5 FAIL; 3 pass-through guards pass | [`1d8412879`](https://github.com/ImIOImI/argo-cd/commit/1d8412879) | `red-green.sh 029c25404 0147fadd9 ./controller/ '^TestMultihub'` → PROVEN |
| Commit-time lookup, helm/OCI skip, init wiring | [`cae847674`](https://github.com/ImIOImI/argo-cd/commit/cae847674): 4 FAIL | [`517c88bc5`](https://github.com/ImIOImI/argo-cd/commit/517c88bc5) | `red-green.sh cae847674 a4f05f0ba ./controller/ '^TestMultihub'` → PROVEN |
| Pin telemetry | [`9ff56a9df`](https://github.com/ImIOImI/argo-cd/commit/9ff56a9df): 2 + 3 FAIL | [`f851f6d73`](https://github.com/ImIOImI/argo-cd/commit/f851f6d73) | `red-green.sh 9ff56a9df 097e3e665 ./controller/multihub/ '^TestPinPlan'` and `… ./controller/ '^TestMultihubPinRevisionReports'` → PROVEN |

Proofs ran against each green commit before its message was amended to
include the proof. The amended commit has the same tree; that's checked
before every push. Full proof output is in each green commit's message.

**Process notes (as they happened):**
- **Fixture bug, caught before the red commit:** the first red run of pair 2
  failed in the fixture, not in the code under test. The fixture assumed the
  app's destination was `test.FakeClusterURL`; it is
  `https://localhost:6443`. Fixed by taking server and key from the
  controller's own `multihubTarget`, then re-run. Every failure was then
  checked for its *reason*, not just its count.
- **Untested code moved out of a green commit:** pair 2's first green draft
  also had the repo-server lookup and the helm/OCI skip, which no red test
  covered. They were removed and became pair 3.
- **Test bug caught by the green run:** pair 4's green run failed only on
  debug-level assertions. logrus only fires hooks for enabled levels, and
  the test helper left the logger at info. The helper was fixed **in the
  red commit**, which hadn't been pushed (amended, red re-verified), not in
  the green commit.
- **Uncovered guard branches**, listed in the green commits: no lookup
  configured, destination or pin errors, and the repo-server returning a
  nil response. All are the same "behave like a single hub" pass-through
  that the tested commit-time-error case exercises.

Tests: 13 new in `controller/multihub` (35 in total) and 15 new in
`controller` (`TestMultihub*`). Full `./controller/` (96s) and
`./controller/multihub/` pass.

## How it was run

Lab scripts are in `hack/multi-hub-lab/`, run from that directory. Same
environment as before.

| Step | Command | Script (pinned) |
|---|---|---|
| Deploy C1 → `argocd-multihub:f851f6d73` | `./deploy-fork.sh` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) |
| Reset telemetry to defaults (left over from the telemetry note's V2) | `./multihub-env.sh ARGOCD_MULTIHUB_LOG_DENIALS- ARGOCD_MULTIHUB_LOG_RENEWALS- ARGOCD_MULTIHUB_LOG_LIFECYCLE- ARGOCD_MULTIHUB_EVENTS-` | [`multihub-env.sh` @ e028e0111](https://github.com/ImIOImI/argo-cd/blob/e028e0111/hack/multi-hub-lab/multihub-env.sh) |
| Confirm settings | `./config.sh` | [`config.sh` @ fad325935](https://github.com/ImIOImI/argo-cd/blob/fad325935/hack/multi-hub-lab/config.sh) |
| Warm both repo caches | `./refresh-both.sh && ./wait-refreshed.sh && ./wait-synced.sh` | [`refresh-both.sh` @ 36e7e2c79](https://github.com/ImIOImI/argo-cd/blob/36e7e2c79/hack/multi-hub-lab/refresh-both.sh), [`wait-refreshed.sh` @ d0811ebad](https://github.com/ImIOImI/argo-cd/blob/d0811ebad/hack/multi-hub-lab/wait-refreshed.sh), [`wait-synced.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/wait-synced.sh) |
| Pin and hub state before E2 | `./state.sh` | [`state.sh` @ 02b42f6bf](https://github.com/ImIOImI/argo-cd/blob/02b42f6bf/hack/multi-hub-lab/state.sh) |
| **E2**: one commit, self-heal on | `./bump.sh` → `f56e377`, 23:29:38Z | [`bump.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/bump.sh) |
| **E7**: both hubs discover a commit together | `./bump.sh && ./refresh-both.sh` → `2afd686`, 23:37:08Z | as above |
| Sync timeline | `SINCE=<w> AFTER=<t> ./syncs.sh` | [`syncs.sh` @ ca2b48900](https://github.com/ImIOImI/argo-cd/blob/ca2b48900/hack/multi-hub-lab/syncs.sh) |
| Lease + pin log | `SINCE=<w> AFTER=<t> [RAW=1] ./lease-log.sh` | [`lease-log.sh` @ 28ef6baba](https://github.com/ImIOImI/argo-cd/blob/28ef6baba/hack/multi-hub-lab/lease-log.sh) |
| Metrics | `./metrics.sh` | [`metrics.sh` @ 33070bb36](https://github.com/ImIOImI/argo-cd/blob/33070bb36/hack/multi-hub-lab/metrics.sh) |
| Raw data | `SINCE=<w> ./collect.sh <dir>` | [`collect.sh` @ 07e28cdae](https://github.com/ImIOImI/argo-cd/blob/07e28cdae/hack/multi-hub-lab/collect.sh) |

**Provenance and tooling fixes:**
- `state.sh` was first run as a one-off command (reading the pin annotations
  and each hub's revision before E2). It was saved afterwards (02b42f6bf) and
  re-run with matching output.
- `syncs.sh` @ 40529be4d exited 1 when the last hub listed had started **no**
  syncs. `grep` finding nothing plus `pipefail` caused this; hub-b's first
  sync-free run is when it showed up. That stopped the evidence chain after
  `syncs.txt` was written, so the first E7 push never happened. Fixed in
  ca2b48900. All E2 evidence was then re-collected with the fixed script,
  and E7 was run.

Raw data: [data/2026-11-14-c1-revision-pin](data/2026-11-14-c1-revision-pin)
(`e2/`, `e7/`).

---

## R1 (E2). One commit, self-heal on: 1 sync, no flapping

Before the push, `./state.sh` showed the pin at `f851f6d` (by hub-a), with
both hubs synced to it.

`syncs.txt`:
```
== hub-a
2026-11-14T23:30:38Z rev=f56e377 selfHealAttempt=0
== hub-b
```
`lease-log.txt`:
```
== hub-a
23:30:37 info  advanced  pin       multihub: advanced the pinned revision to f56e377…
23:30:38 info  acquired  auto-sync multihub: operation lease acquired (generation 17)
23:30:51 info  released  operation multihub: released operation lease
== hub-b
23:30:38 info  adopted   pin       multihub: using pinned revision f56e377… (pinned by hub-a) instead of this hub's f851f6d…
23:30:39 info  held      auto-sync Skipping auto-sync: operation lease held by hub-a (rechecking in 15s)
23:30:54 info  adopted   pin       multihub: using pinned revision f56e377… (pinned by hub-a) instead of this hub's f851f6d…
```

- **1 sync, by hub-a.** hub-b started none. In the baseline, hub-b reverted
  to its old commit 5 times; with only the lease (C2), 4 times.
- **The skew is still there, but it no longer causes damage.** hub-b's own
  resolution was still the old `f851f6d`. Each time, it adopted the pin and
  compared at `f56e377` instead. It adopted within **1s** of hub-a moving
  the pin (23:30:37 → 23:30:38), triggered by the spoke change from hub-a's
  sync.
- **The two mechanisms work together:** at 23:30:39 hub-b, now comparing at
  the pin, saw OutOfSync because hub-a's sync was still running. It was
  refused the lease, and at 23:30:54 it found the app in sync. Without C2,
  that would have been a duplicate sync of the right revision. Without C1,
  it would have been a revert.
- Both hubs ended `Synced f56e377`, and the spoke ended on generation 10.

## R2 (E7). Both hubs discover a commit together: still 1 sync

```
== hub-a
2026-11-14T23:37:09Z rev=2afd686 selfHealAttempt=0
== hub-b
```
hub-a advanced the pin and acquired the lease at 23:37:09Z, then released
at 23:37:24Z. **1 PreSync hook Job** for `2afd686`. Both hubs ended `Synced
2afd686`.

## R3. Telemetry finding: the refusal dedup never expires

In E7, hub-b's info log shows **nothing**. Its metric shows it **was**
refused 4 times (`held{path="auto-sync"}`: 3 after E2, 7 after E7). The
refusals went to debug because the "log on change" memory still held "held
by hub-a" from E2, 7 minutes earlier. A hub that never takes the lease itself
never resets that memory, so a new **episode** of refusals looks like a
repeat. Behavior was correct; visibility was not.

**Fix (next red → green):** forget a remembered refusal after a quiet
period, e.g. one lease duration with no further refusal, so each episode
logs once.

**Fixed 2026-11-20** (default 2 min of quiet, configurable): see
[2026-11-20-refusal-dedup-expiry.md](2026-11-20-refusal-dedup-expiry.md).

---

## Known gaps

1. **Ordering by commit time, not ancestry.**
   - The pin never moves to an older-dated commit. A **force-push or reset
     to an earlier commit** therefore leaves both hubs on the old pin until
     a newer commit lands.
   - Commits with **skewed author clocks** can be misordered.
   - The real fix is an ancestry check on the repo-server (a new RPC). C5
     (a deliberate override pin, for rollback) gives an operator escape
     hatch.
2. **Multi-source apps aren't pinned.** They behave like stock, with skew
   flapping. The pin would need to be a vector of revisions.
3. **One repo-server metadata call per comparison.** It's cached in the
   repo-server, but it is an extra RPC per refresh per app when coordination
   is on.
4. **The re-comparison costs a second manifest render** while a hub is
   behind the pin, i.e. only during the skew window.
5. **No live test yet of a pin error** (repo-server down, spoke
   unreachable). Unit-tested only.

## Scoreboard changes

| Problem | Stock v3.6.1 | Fork, C2 | Fork, C1 + C2 |
|---|---|---|---|
| Syncs per commit, self-heal on (E2) | 11, overlapping | 9, serialized | **1** |
| Time the hubs disagree after a commit (E2) | ~4m50s | flapping ~1.5 min | **~1s** (pin set → adopted) |
| Spoke shows the *old* generation after a commit | possible | possible | **no revert observed** |
| Full syncs when both hubs discover a commit together (E7) | 2 | 1 | **1** |
