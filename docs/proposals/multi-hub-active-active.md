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
4. Share the work. Two hubs do about one hub's worth of reconciliation
   between them, not two. Each hub refreshes a given app about half as
   often, while together the app is refreshed as often as a single hub
   would. (Added 2026-11-21; see C7.)

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

- **Correction (2026-11-20, E8):** the same name alone doesn't adopt a
  running hook. gitops-engine treats a hook with no entry in the
  operation's per-resource results as not started, and with
  `BeforeHookCreation` it deletes and re-creates the live one. Takeover
  has to carry the results too (C3 mirrors the whole `OperationState`).
  See [research/2026-11-20-c3-takeover.md](../../research/2026-11-20-c3-takeover.md), R2.

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

*As built (2026-11-20):* the override is annotations on the app's Lease.
Hubs don't compare at the override revision. Instead, every hub's
**auto-sync stands down** while an override exists, which is the multi-hub
form of Argo's own "rollback requires auto-sync off". Both hubs keep
reporting OutOfSync against HEAD. A user sync to the `targetRevision` on any
hub clears it. See
[research/2026-11-20-c5-override.md](../../research/2026-11-20-c5-override.md).

### C6. Notifications
Only the hub that ran an operation sends its notifications. For
health-change notifications, a separate lease per app decides which hub
sends.

### C7. Shared refresh: refresh once, share the result

Without C7, every hub refreshes every app on its own timer (default 3
minutes). A refresh resolves the commit from Git, renders the manifests,
diffs every resource against live state, computes health and writes the
status. So two hubs do two hubs' worth of that work for one hub's worth of
benefit. C7 has the hubs split it.

**Publish.** After a full refresh, a hub publishes what it found:
- **Lease annotations, kept small:** `refreshed-by`, `refreshed-at`, the
  revision compared, the **definition fingerprint** (below) and a hash of
  the result.
- **The result itself:** sync status and revision, health, per-resource
  status, conditions and summary. This goes into a separate object
  (`<lease>-result`, gzipped) and is rewritten only when its hash changes.

**Skip.** When a hub's *periodic* refresh comes due (the soft-expiry case,
nothing else), it checks the app's Lease in a local watch cache. It skips
the comparison if:
- another hub published a refresh within this hub's refresh period;
- of the same definition fingerprint;
- at the revision currently pinned (C1);
- and the watch is healthy.

When it skips, it fetches the published result only if its hash differs
from what this hub already shows. It applies the result to its own
Application, rebuilds the resource tree from its live cluster watch (cheap,
the same path upstream uses for status-only refreshes), and sets
`reconciledAt` to now.

**Always local, never skipped:** user-requested refreshes, refreshes caused
by spec changes, event-driven refreshes, operations, and anything when the
cache is unhealthy or stale.

**How the work splits.** Each app is refreshed by whichever hub's timer comes
due first. The other finds a fresh result and skips. For a single app, one
hub may keep doing the work for as long as its timer stays ahead. Across
many apps, with independent timers, the work splits about evenly, and each
app is still refreshed once per period in total. It isn't strict per-app
alternation. It's measured, not assumed (E11).

**Failover is automatic.** A hub that stops publishing stops being fresh, so
the other hubs refresh everything themselves at their normal rate. Nothing
is reassigned and no step is needed.

**The watch.** Each hub's controller keeps one informer per spoke on Leases
in the coordination namespace with the multihub label. Only the "may I skip"
hint reads the cache. Everything else (lock, claim, override, publish) still
reads and writes through the API with compare-and-swap. A stale cache costs
a duplicate refresh, never a wrong sync.

**The definition fingerprint** is the first half of the divergence check
(feature 1 in the 2026-11-21 discussion). It's a hash of the parts of the
Application that decide what is deployed:
- source(s), destination (resolved server and namespace) and project;
- sync options, ignoreDifferences and managed namespace metadata;
- but not auto-sync settings, labels or annotations.

C7 only shares between hubs whose fingerprints match. Pausing on mismatch
is C9.

**Costs:**
- One Lease write per refresh actually done, and the result object written
  when the result changes.
- The skip itself costs a map lookup.
- **Event-driven refreshes aren't shared yet.** Both hubs still react to
  resource changes. Sharing those is a follow-up, if measurements show it
  matters.
- The UI's per-resource diff on a skipping hub is as of its own last full
  refresh. Status and tree are current.

### C8. Removing an application requires every hub to agree

When an ApplicationSet on one hub stops generating an Application (it's gone
from Git), that hub doesn't delete it. Instead:

1. **It records absence.** It writes `absent-by.<hub>` on the app's Lease.
2. **Deletion waits for agreement.** The app is deleted only once every
   member hub has recorded it absent. A Git error or template bug on one hub
   therefore never deletes anything.
3. **Coming back cancels it.** If the app reappears before then, that hub
   clears its absence record.
4. **Down hubs block deletion.** A hub that is down counts as not agreeing,
   so deletions wait for it until an operator removes it from membership.

Not built yet.

### C9. Divergence and pauses (2026-11-21 discussion)

- **Divergence:** hubs whose definition fingerprints differ pause syncing
  for that app until they match. The pause message names the differing
  fields.
- **Global pause:** an operator can pause an app on every hub ahead of time.
- **Single-hub pause:** an operator can pause one hub. A paused hub's
  fingerprint doesn't count toward agreement, which gives a deliberate way
  to let one hub's definition win while the other is fixed.

Not built yet. C7 builds the fingerprint they all need.

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
   and expect hub-b to adopt the hook rather than run it again. *Status:
   implemented test-first; E8 (hub-a killed mid-hook) runs the hook once
   instead of twice, and a returning hub takes the result instead of
   resuming its stale operation. The mirror lives on the Lease
   (annotation), not `status.operation`. See
   [research/2026-11-20-c3-takeover.md](../../research/2026-11-20-c3-takeover.md)
   and the follow-up
   [research/2026-11-20-c3-gaps.md](../../research/2026-11-20-c3-gaps.md)
   (manual syncs, mid-rollout death, takeover in 3–5s after expiry).*
5. **C5**: *done, see the C5 note above.*
6. **C7**, shared refresh, measured as E11. *Status: done; each hub does
   54–58% of its unshared refresh work, and failover needs no step. See
   [research/2026-11-21-c7-shared-refresh.md](../../research/2026-11-21-c7-shared-refresh.md).*
   Then C8 and C9, then C4 and C6.

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
