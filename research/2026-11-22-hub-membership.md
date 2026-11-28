# Hub membership: declared in Git, live on every hub

**Date:** 2026-11-22 · **Fork commits:** `cc59723d8` … `57ca73492` (10 red → green pairs)
· **Lab image:** `argocd-multihub:57ca73492` (after the fix in R2)

## Summary

Which hubs are members, and in which state, is now declared in Git as a
`HubMembership` resource that every hub's root app syncs. The design and the
decisions behind it are in the proposal, under "Membership and voting".

| Check (3-hub lab) | Result |
|---|---|
| **A**: hub-c set to `watching` in Git | applied on all three hubs within 22s; on the next commit hub-c started **0** syncs (hub-a 3, hub-b 8; 1 hook per app); hub-c still showed all apps Synced/Healthy |
| **R2**: a watching hub receiving the next Git change | **failed at first:** hub-c's own root app was gated too, so it could never be made active again. **Fixed** test-first (`b5f145ac9` → `57ca73492`), then verified live |
| **B**: hub-c set back to `active` in Git | hub-c ran syncs again (2 of 11; 1 hook per app) |
| **C**: hub-c retired mid-sync (its self-registration label changed to `role=retired`) | the sync it was running **finished on hub-c** (no takeover); on the next commit it started **0** syncs |
| refresh period via `spec.refreshPeriod` (120s → 90s) | applied on all three hubs (on hub-c after the R2 fix) |
| **Open anomaly** (R4) | after Demo C's second commit, 9 of 11 apps were brought to the new commit by **partial** syncs, so their PreSync hooks didn't run. Cause not yet known |

## What was built

**The `HubMembership` CRD** (`manifests/crds/hubmembership-crd.yaml`, part of
the fork's CRD install):
- `spec.refreshPeriod`, `spec.forgetAfter` (`0` or a bare `0` = never;
  combinable `d`/`h`/`m`/`s`) and `spec.members` (by `id` or label
  `selector`, each `active` or `watching`).
- Validated by schema and CEL at apply time. A `--dry-run=server` check
  against a real API server rejected id+selector, an unknown state, an
  empty list and malformed durations, each with a clear message.

**Parsing and resolution** (`controller/multihub/membership.go`):
- An `id` entry overrides selectors.
- Conflicting selectors are an error, so the hub keeps its last valid
  membership.
- Quorum: everyone for 1–2 active hubs, a majority for 3+.
- Hubs declared by id count even without a record (the strictest reading).
- Silent hubs are forgotten after `forgetAfter`.

**The watcher** keeps the last valid `HubMembership`: an invalid version keeps
the previous one, and deleting it returns to today's behavior.

**Each hub's own state** is resolved from its `ARGOCD_MULTIHUB_HUB_ID` and the
labels on its self-registration (the cluster Secret for
`kubernetes.default.svc`).

**Hub records:**
- Each hub publishes `hub.<id>` on every cluster it manages (id,
  self-registration labels, last seen) every 30s, members or not.
- Hubs read each other's records from the Lease watch.

**The gate.** A `watching` or non-member hub:
- starts no auto-sync or new operation (with a reason);
- doesn't take over others' operations;
- doesn't publish shared refreshes.

Running operations finish, which is the drain. Apps that deploy into the
hub's own cluster are never gated (R2).

**The refresh period** comes from `spec.refreshPeriod` first, then
`argocd-cm`, then the startup value.

**Not built yet:** voting itself (C8/C9), balanced sharing (C7b), and the
`HubMembership` status (the printer columns exist but stay empty).

## TDD record

All pairs were proven with `hack/tdd/red-green.sh`, and the proof is in each
green commit's message.

| # | Red | Green | What |
|---|---|---|---|
| 1 | `cc59723d8` | `ce76779e3` | CRD, `ParseMembership`, `ParseDuration` |
| 2 | `f0bcb71cf` | `a06fd0b46` | `StateOf`, `Resolve`, `QuorumFor` (green also accepts a bare `forgetAfter: 0`; see below) |
| 3 | `d62710d63` | `7c5b3dbcb` | `MembershipWatcher` (last valid version); race-tested |
| 4 | `86916a733` | `217522d36` | the hub's own state from `HubMembership` and self-registration |
| 5 | `2cf564383` | `6aec1b9c8` | hub records: publish and read from the cache |
| 6 | `11bc32271` | `9fe0a5583` | declared ids count without a record |
| 7 | `775fbae46` | `a5de85a4b` | heartbeat publishing hub records |
| 8 | `509b4a0aa` | `dc9d38554` | watching and non-member hubs start no work; running syncs finish |
| 9 | `54baf2686` | `796a89d2e` | `spec.refreshPeriod` first |
| 10 | `b5f145ac9` | `57ca73492` | **fix:** never gate apps that deploy into the hub's own cluster (R2) |

**Notes on the record:**
- **Pair 2.** The red test wrote `forgetAfter: 0` unquoted, as a user
  would. YAML reads that as a number, and the string-only CRD schema would
  have rejected it. The green accepts a bare `0` in the parser and in the
  CRD (int-or-string plus a CEL rule), re-checked with `--dry-run=server`.
- **Pair 10 was done twice. Two mistakes:**
  - The first red asserted the root's sync `Succeeded`, which the unit-test
    controller can't do (it can't build an in-cluster client), so the test
    still failed after the fix.
  - My command chain didn't stop on that failure. It committed the green
    anyway and deployed it to the lab. The proof script reported "not
    proven", so nothing was pushed. Both commits were redone before
    pushing: the red now asserts the sync isn't **refused**, and red and
    green were re-proven.
  - For about 10 minutes the lab ran an unproven build (`a631da52c`, the
    same code as `57ca73492`). It was replaced with the proven build before
    any measurement in this note.

`go test ./controller/...` passes at `57ca73492`.

## How it was run

Lab scripts are in `hack/multi-hub-lab/` and were run from that directory, on
three hubs.

| Step | Command | Script (pinned) |
|---|---|---|
| Deploy (also installs the CRD) | `./deploy-fork.sh` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh), hub loop from [`lab-hubs.sh` @ 01750a440](https://github.com/ImIOImI/argo-cd/blob/01750a440/hack/multi-hub-lab/lab-hubs.sh) |
| Self-registration per hub (`hub-id` = the hub, `role=hub`) | `./self-register.sh` | [`self-register.sh` @ e12b24539](https://github.com/ImIOImI/argo-cd/blob/e12b24539/hack/multi-hub-lab/self-register.sh) |
| Shared `HubMembership` (every `role=hub` hub active, 120s) | in [`bootstrap/hubmembership.yaml` @ e12b24539](https://github.com/ImIOImI/argo-cd/blob/e12b24539/hack/multi-hub-lab/bootstrap/hubmembership.yaml), synced by each root app; roots hard-refreshed with `kubectl annotate` (one-off) | |
| hub-c's first root sync failed (its CRD arrived after its root first tried; see R1); re-triggered | `kubectl patch application lab-root` with a sync operation (one-off) | |
| Each hub's view and the hub records | `./membership.sh` | [`membership.sh` @ e12b24539](https://github.com/ImIOImI/argo-cd/blob/e12b24539/hack/multi-hub-lab/membership.sh) |
| **A**: hub-c `watching` (commit 455017e4b), then a commit, 5 min | `./bump.sh && ./refresh-both.sh`, `SINCE=10m ./ops-count.sh <start>` | [`ops-count.sh` @ 40cf8a79f](https://github.com/ImIOImI/argo-cd/blob/40cf8a79f/hack/multi-hub-lab/ops-count.sh), [`bump.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/bump.sh) |
| **R2**: refresh period 120s → 90s (commit 12951a6a1); each hub's `spec.refreshPeriod` and root status, 60s later; again after the fix | one-off `kubectl get` | |
| **B**: hub-c active again (commit 355acd64a), then a commit, 5 min | as A | |
| **C**: `./hook-sleep.sh 120`, a commit, hub-c refreshes first and takes lab-app; mid-hook `ROLE=retired HUBS=hub-c ./self-register.sh`; after 200s, `./opstate.sh` and takeover lines; then `./hook-sleep.sh 5`, a commit, 5 min | `./refresh.sh hub-c`, `./wait-lease.sh hub-c`, `./wait-hook.sh`, `./opstate.sh` | [`refresh.sh`](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/refresh.sh), [`wait-hook.sh`](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/wait-hook.sh), [`opstate.sh`](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/opstate.sh) @ fcc7bafb4 |
| R4 evidence: every `Initialized new operation` after the second commit, classified full or partial | `kubectl logs` + a one-off filter; raw lines in `demo-c-second-commit-operations.jsonl` | |

Raw data: [data/2026-11-22-hub-membership](data/2026-11-22-hub-membership).

---

## R1. Membership arrives through Git

After `./self-register.sh` and the root syncs, all three hubs held the same
`HubMembership`, and all three published records:

```
hub-a   {"multihub.argoproj.io/hub-id":"hub-a","multihub.argoproj.io/role":"hub"}
hub-b   {"multihub.argoproj.io/hub-id":"hub-b","multihub.argoproj.io/role":"hub"}
hub-c   {"multihub.argoproj.io/hub-id":"hub-c","multihub.argoproj.io/role":"hub"}
```

**hub-c's root failed its first sync:**
`failed to discover server resources for group version multihub.argoproj.io/v1alpha1`.

- Its CRD was installed (by `deploy-fork.sh`) moments after its root first
  tried, and Argo doesn't retry a failed sync of the same revision by itself.
  A manual re-sync fixed it.
- **Requirement for real installs:** the CRD must be in place before the
  root app first syncs a `HubMembership`. In practice, install the fork's
  CRDs with Argo itself, as `deploy-fork.sh` does, before bootstrapping the
  root.

## R2. Found live: a watching hub couldn't be made active again

After Demo A made hub-c `watching`, a change to the refresh period
(120s → 90s) reached hub-a and hub-b but not hub-c:

```
hub-a: refreshPeriod=90s  root=Synced/Succeeded
hub-b: refreshPeriod=90s  root=Synced/Succeeded
hub-c: refreshPeriod=120s root=OutOfSync/Succeeded
```

- **Cause:** the gate applied to every app on hub-c, including its own root
  app (destination `kubernetes.default.svc`). So the Git change that would
  make hub-c active again could never be applied. `watching` would have
  locked itself in.
- **Fix:** apps that deploy into the hub's own cluster are never gated.
  Membership governs shared work; a hub's own bootstrap and configuration
  are its own business.
- **After deploying `57ca73492`:**

```
hub-a: refreshPeriod=90s root=Synced/Succeeded
hub-b: refreshPeriod=90s root=Synced/Succeeded
hub-c: refreshPeriod=90s root=Synced/Succeeded
```

## R3. Watching, active again, retired mid-sync

- **A (watching):** hub-c ran 0 operations; hub-a ran 3 and hub-b 8, one
  per app and one hook per app. All apps were at generation 24, and hub-c
  showed all 11 Synced/Healthy.
- **B (active again):** hub-a 4, hub-b 5, hub-c 2; one hook per app.
- **C (retired mid-sync):**

```
hub-c retired (role=retired) at 16:11:11Z   (its lab-app hook started 16:11:03)
hub-c Succeeded startedAt=16:11:02 finishedAt=16:13:29 | successfully synced (all tasks run)
lease holder=- ; mirror hub=hub-c running=False phase=Succeeded
hub-a takeovers of lab-app: 0 ; hub-b takeovers of lab-app: 0
--- next commit
hub-c: operations=0
hub record: hub-c {"multihub.argoproj.io/hub-id":"hub-c","multihub.argoproj.io/role":"retired"}
```

The running sync finished where it was (the drain), and nothing new
started.

## R4. Open: a new commit applied by partial syncs, without hooks

**Update 2026-11-28:** reproduced on stock Argo CD v3.6.1, so it's an
upstream bug the fork makes more frequent. See
[2026-11-28-r4-skipped-hooks.md](2026-11-28-r4-skipped-hooks.md).


After Demo C's second commit (`4c6e414`: generation 27 and the hook back to
5s), every app reached generation 27, but:

```
hub-a 16:14:31 lab-app full | 16:14:32 lab-6 full | 16:15:33 lab-9 partial | 16:15:38 lab-4 partial
hub-b 16:14:36 lab-1 partial | lab-5, lab-2, lab-3, lab-7, lab-8, lab-10 partial (16:14:58-16:16:06)
hook jobs created: 2 (lab, lab-6)
```

- **9 of 11 apps were updated by partial syncs,** the kind auto-sync issues
  when it has already attempted that revision and self-heals only the
  resources that differ. Hooks don't run for partial syncs, so those apps'
  PreSync hook was skipped for this commit.
- **Every earlier run in this session** had one *full* sync and one hook per
  app.
- **What was different:**
  - the commit landed while syncs from the previous commit (120s hooks)
    were finishing;
  - it changed the hook itself;
  - hub-c had just been retired.
- **Possibly related:** it resembles the stale-comparison self-heal noted in
  the C5 note, which also involved the wave-1 ConfigMap.
- **Not yet known** whether this is upstream behavior or caused by the
  fork. It should be reproduced, and compared with `MULTIHUB=off`, before
  C8. A skipped hook is the kind of silent wrong outcome the project is
  meant to prevent.

## Known gaps

1. **R4**, above.
2. **`HubMembership` status** isn't written yet, so `kubectl get
   hubmembership` shows empty columns.
3. **Watching and non-member hubs** log their refusals at debug only. A
   per-change info line or Event would make the state visible.
4. **Voting (C8/C9) and balanced sharing (C7b)** aren't built.
