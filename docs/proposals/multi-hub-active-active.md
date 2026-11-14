---
title: Multi-hub active/active Argo CD
authors:
  - "@ImIOImI"
sponsors:
  - TBD
reviewers:
  - TBD
approvers:
  - TBD
creation-date: 2026-11-14
last-updated: 2026-11-14
---

# Multi-hub active/active Argo CD

**Status: research spike** on fork `ImIOImI/argo-cd`, branch `spike/multi-hub`.
Code references are against upstream `ae2e78422` (master, Nov 2026).

## Summary

Two or more Argo CD instances ("hubs"), each in its own cluster, manage the
**same Applications on the same destination clusters** with every
application controller active. A user can work from either hub, losing a hub
does not stop reconciliation, and a hub can be retired by adding its
replacement next to it and then turning the old one off.

## Goals

1. Log into either hub and manage the same workloads: status, sync, rollback.
2. Losing a hub has no operational impact. Reconciliation keeps going on the
   survivor with no failover step.
3. Migrate hubs safely: bring up the new hub, prove it agrees with the old
   one, remove the old one.

## Non-goals (for now)

- Keeping Application or AppProject **specs** identical across hubs. The spike
  assumes apps are defined in Git (app-of-apps or ApplicationSets) on every hub.
- Hubs on different Argo CD versions reconciling the same app. Version skew
  is detected and fenced off; it is not supported.

## Why upstream fights today

Diffing compares **content**. Both hubs apply with the same identity: field
manager `argocd-controller`, the same tracking annotation and the same
`installationID`. So when one hub applies exactly what the other would apply,
the other sees no diff. Churn comes only from the two hubs **wanting
different things** or **acting twice**. The code paths behind this:

### F1. Each hub resolves the branch on its own schedule
- `processAppRefreshQueueItem` (`controller/appcontroller.go:2139`) builds
  `revisions` from `spec.source.targetRevision`. The repo-server resolves the
  branch to a SHA during manifest generation
  (`reposerver/repository/repository.go:3041`, `ResolveRevision` at `:3306`).
  How fresh that is depends on each hub's reconciliation timeout, revision
  cache and webhooks.

### F2. A spoke change makes the *other* hub compare against its *old* revision
- `handleObjectUpdated` (`controller/appcontroller.go:424`) queues a
  `CompareWithRecent` refresh whenever a managed resource changes.
- At `CompareWithRecent`, the refresh uses `app.Status.Sync.Revision`, the
  last SHA **this hub** resolved (`appcontroller.go`, in
  `processAppRefreshQueueItem`, "if comparisonLevel == CompareWithRecent").
- So when hub A applies commit N+1, hub B immediately compares the live state
  against N, sees OutOfSync and, with self-heal on, applies N again. Hub A then
  does the same in reverse. The flapping lasts until hub B's next
  `CompareWithLatest` resolves N+1, and self-heal backoff
  (`selfHealRemainingBackoff`, `:3174`) slows each round.
- **Hypothesis H1:** skew churn needs `selfHeal: true`. Without it,
  `alreadyAttemptedSync` (`:3139`) sees a successful sync at N and skips.
  Lab experiment E4.

### F3. "Already synced?" only knows about this hub's own syncs
- `autoSync` (`:2982`) and `alreadyAttemptedSync` check
  `app.Status.OperationState`, which lives on **this hub's** Application CR.
  Hub B never learns that hub A synced. If both see OutOfSync at the same
  moment, both call `argo.SetAppOperation`, and both run full syncs, hooks
  included.

### F4. Sync progress lives on the hub that started it
- An operation is a state machine that resumes on every pass. Each
  `processRequestedAppOperation` (`:1603`) call re-enters `SyncAppState`
  (`controller/sync.go:160`), which rebuilds progress from
  `state.SyncResult.Resources` (`sync.go`, `initialResourcesRes`).
- If the hub running a sync dies, the other hub has none of that state and
  would start over.

### F5. Generated hook names are deterministic, which helps takeover
- `gitops-engine/pkg/sync/sync_context.go:1029`: a hook that uses
  `generateName` is named `<generateName><sha7>-<phase>-<startedAt unix>`.
- If a takeover hub reuses the original operation's `revision` and
  `startedAt`, it computes the **same hook names** and adopts the running Job
  instead of creating a second one. Two hubs that start separate operations
  get different `startedAt` values, so their hooks are duplicated (F3).

### F6. Manual operations are local
- `Sync` and `Rollback` (`server/application/application.go:2144`, `:2341`)
  write an operation onto the hub's own Application CR. A rollback on hub B
  is not visible to hub A, and hub A's self-heal reverts it.

## Design: coordinate through the spoke

One `SyncCoordination` object per Application lives **on the destination
cluster** (`argocd-coordination` namespace). Both hubs already need write
access there. The spoke's API server, backed by etcd, gives linearizable
compare-and-swap through `resourceVersion`, so:

- there is no new highly available datastore to run;
- the lock has the same failure domain as what it protects: if a hub can't
  reach the spoke, it can't sync to it either;
- hubs never talk to each other, so a network split between them can't
  cause split brain.

```yaml
apiVersion: coordination.argoproj.io/v1alpha1
kind: SyncCoordination
metadata:
  name: <installationID>.<appNamespace>.<appName>
spec:
  pinnedRevisions: [3f9c1e...]     # agreed SHA(s), one per source
  pinnedBy: hub-b
  override: false                  # true = rollback/manual pin; don't auto-advance
status:
  lease:
    holder: hub-a
    generation: 42                 # fencing token, bumped on every acquire
    renewedAt: ...
    ttl: 30s
  operation:                       # mirrored OperationState for takeover (F4)
    startedAt: ...
    revision: 3f9c1e...
    phase: Running
    resources: [...]               # trimmed SyncResult.Resources
  renders:                         # per-hub manifest hash at pinnedRevisions
    hub-a: {hash: sha256:..., argocdVersion: v3.6.1}
    hub-b: {hash: sha256:..., argocdVersion: v3.6.1}
```

### C1. Revision agreement (fixes F1, F2)
Hook point: right after `CompareAppState` in `processAppRefreshQueueItem`.
- After resolving HEAD to `sha`, the hub compare-and-swaps `pinnedRevisions`.
  It may only move the pin forward, to a descendant of the current pin
  (`git merge-base --is-ancestor`, which needs a new repo-server RPC; the
  spike can approximate with commit dates from `GetRevisionMetadata`).
- If the pin is ahead of what this hub resolved, the hub re-runs the
  comparison at the pin. That costs a second comparison, but only during
  the skew window.
- `CompareWithRecent` uses the pin, not `app.Status.Sync.Revision`. This
  closes the F2 window entirely.

### C2. Operation lease (fixes F3)
Hook points: `autoSync` before `argo.SetAppOperation`, and
`processRequestedAppOperation` before `SyncAppState`.
- A hub must hold the lease to start or continue an operation, and renews
  it on every requeue.
- Before each write phase, the holder checks that `generation` hasn't moved.
  If it has, the hub lost the lease and aborts.
- A hub that sees OutOfSync but can't get the lease skips auto-sync and
  shows "Syncing (hub-a)".

### C3. Takeover (fixes F4, uses F5)
The holder mirrors a trimmed `OperationState` into `status.operation` after
each pass. When the lease expires, another hub acquires it with a new
`generation`, puts the mirrored state into its own Application, and resumes.
Same `revision` + `startedAt` gives the same hook names, so running hooks are
adopted, not run again.

### C4. Render-hash divergence
After manifest generation, each hub records a hash of the normalized target
manifests at the pinned revision. If the hashes differ (non-deterministic
Helm, different tool versions), the app is marked **Diverged**: only the
lease holder may sync, and the other hub raises a condition instead of
self-healing. This also works as a **migration pre-flight**: a new hub lists
every app it would render differently before it touches anything.

### C5. Coordinated manual operations (fixes F6)
`Rollback` and sync-to-revision set `pinnedRevisions` with `override: true`
on the spoke. Every hub then honors the override until it's cleared by a
sync to HEAD or by removing the override. Parameter overrides would be
stored the same way, or disabled.

### C6. Notifications
Only the hub that ran an operation sends its notifications. For
health-change notifications, a separate lease per app decides which hub
sends.

## Spike plan

1. **Baseline lab** (`hack/multi-hub-lab`): stock Argo on two hubs, one spoke.
   Run experiments E1–E5 and measure churn and duplicate hooks. *Status:
   done, see [research/2026-11-14-baseline-stock-argo.md](../../research/2026-11-14-baseline-stock-argo.md).*
2. **C2 first**: the operation lease alone should remove duplicate syncs and
   duplicate hooks. It is the smallest change that's useful. *Status:
   implemented (`controller/multihub`); removes duplicate full syncs, doesn't
   stop skew flapping. See
   [research/2026-11-14-c2-operation-lease.md](../../research/2026-11-14-c2-operation-lease.md).*
3. **C1**: revision pin. Re-run E2 and expect zero flapping. *Status:
   implemented test-first; E2 went from 9 syncs (C2 only) to 1. See
   [research/2026-11-14-c1-revision-pin.md](../../research/2026-11-14-c1-revision-pin.md).
   The pin lives on the Lease (annotations), not a separate
   `SyncCoordination` object, and is ordered by commit time, not ancestry.*
4. **C3**: kill hub-a mid-sync, including during a running PreSync hook,
   and expect hub-b to adopt the hook rather than run it again.
5. **C4, C5, C6.**

Keep all coordination logic in a new `controller/multihub` package behind an
interface, with small calls into the controller. That keeps the rebase onto
each upstream minor release cheap, and gives the shape an upstream proposal
would need.

## Open questions

- Coordination object granularity: one per app is simple, but a large
  multi-tenant spoke could have thousands. Alternatives are one per
  (spoke, appset) or a sharded set of Leases.
- Apps that deploy to the hub's own cluster (`in-cluster`): the coordination
  object lives on that hub, which is fine because it can't outlive it.
- Multi-source apps: pin each source on its own, or pin the set as one
  vector? The set as one vector is likely the better choice.
- What should a hub do while the spoke is unreachable for coordination but
  still reachable for reads? Proposed: keep computing status, never write.
- RBAC: hubs need write access to `SyncCoordination` on every spoke. That's
  covered by the existing cluster-admin-style credentials, but needs explicit
  rules under impersonation.
