# C7: two hubs share the refresh work

**Date:** 2026-11-21 · **Fork commits:** `a788bee77` … `bb24edf58` (5 red → green pairs)
· **Lab image:** `argocd-multihub:be596a122`

## Summary

Before C7, every hub refreshed every app on its own timer: two hubs did two
hubs' worth of work. With C7, a hub whose periodic refresh comes due uses
another hub's fresh, published result instead of comparing again.

E11: 11 apps, both hubs on a 60s refresh period, 10-minute windows.

| | Sharing off (control) | Sharing on | Sharing on, hub-b stopped |
|---|---|---|---|
| hub-a full refreshes | 63 | **34** (+30 skipped) | **70** (back to full rate) |
| hub-b full refreshes | 65 | **38** (+25 skipped) | n/a |
| Total full refreshes | 128 | **72** (56%) | 70 |
| How often some hub fully refreshed each app | each hub ~every 104s | **~every 92s** | ~every 94s |
| Syncs per app per commit | n/a | **1**, by one hub (4 apps hub-a, 7 hub-b) | n/a |

The control's 9m40s window and the failover's 9 minutes are scaled to 10
minutes.

- **Work per hub drops to 54–58%** of what it did alone. The total is 56% of
  two hubs' worth rather than 50%; the excess is analysed in R1.
- **Each app is still refreshed at least as often** as one hub alone would
  refresh it.
- **Failover needs no step.** With hub-b stopped, hub-a found no fresh
  results and went straight back to refreshing everything itself.
- **The work alternates per app.** I expected each app to stick with
  whichever hub's timer ran ahead. Instead, Argo's refresh jitter keeps
  reshuffling the timers, so each hub refreshed each app about half the
  time (lab-1: hub-a 3 full, 3 skipped). That's closer to strict
  alternation than the design note predicted.

## Design (as built)

The design is in the proposal, under C7. What was built:

1. **Publish.** After every full refresh of a shareable app (single-source
   git, the same apps the pin covers) with no error conditions, a hub
   publishes:
   - **On the app's Lease,** five small annotations: who, when, the
     revision compared, the **definition fingerprint**, and a hash of the
     result.
   - **In the ConfigMap `<lease>-result`,** gzipped and rewritten only when
     it changes: sync status, health, per-resource status, source types and
     the delete-hook flags.
2. **Watch.** Each hub's controller keeps one informer per spoke on the
   coordination Leases (namespace plus label). It's started on first use,
   and isn't trusted until synced, or for 30s after a watch error.
3. **Skip.** A refresh is skipped only when all of these hold:
   - It's due only because the periodic period ran out: no user request,
     no spec change, no hard refresh, and no pending controller request
     (checked without consuming it).
   - The cache shows **another** hub's record within this hub's refresh
     period, at the currently pinned revision, with the same fingerprint.
   - The result can be fetched and its hash matches.
   - Its delete-hook flags match this hub's finalizers.

   When it skips, the hub applies the result, rebuilds its resource tree
   from its own live watch, sets `reconciledAt` to now and persists. That's
   counted as `argocd_multihub_lease_decisions_total{path="refresh",event="shared"}`,
   and the `Reconciliation completed` line carries `multihub.shared=true`.
4. **The fingerprint** hashes the deployment-defining parts of the
   Application: sources, destination resolved to its server, project, sync
   options, ignoreDifferences and managed namespace metadata. It leaves out
   auto-sync settings, labels and annotations. C9 will reuse it for
   divergence detection.
5. **The setting** `ARGOCD_MULTIHUB_SHARE_REFRESH` (default `true`) turns
   sharing off. That's how the control was run.

## TDD record

All pairs were proven with `hack/tdd/red-green.sh`, and the proof is in each
green commit's message.

| # | Red | Green | What | Red failures |
|---|---|---|---|---|
| 1 | `a788bee77` | `75ad18577` | `PublishRefresh` / `FetchResult` / `RefreshRecordOf` | 6 |
| 2 | `b07f58d25` | `3ab90c3cf` | Lease watch cache (`Cached`); race detector, 5 runs | 5 |
| 3 | `1864cfa2e` | `439091c9d` | fingerprint; publish after a full refresh | 2 |
| 4 | `b4dbeee41` | `9b8bbd6b6` | the skip; `ARGOCD_MULTIHUB_SHARE_REFRESH` | 1 in each package (positive case; the six "compare for itself" cases are guards that pass at red) |
| 5 | `f694f0b50` | `bb24edf58` | don't skip with a pending controller request; rebuild the tree | 2 |

**Notes on the record:**
- **Pair 4.** The "pending controller request" rule was first written into
  pair 4's green commit without a test. Before committing, it was taken out
  and given its own pair (5).
- **Pair 5's red** also fixes a test helper that could return before the
  other hub's record arrived. It waited for a matching record without
  checking who had published it.
- **Not covered by tests:**
  - `multihubHasErrorCondition` (no publishing after a comparison with
    errors);
  - the debug log on a failed publish.

`go test ./controller/...` passes at `bb24edf58`.

## How it was run

Lab scripts are in `hack/multi-hub-lab/` and were run from that directory.

| Step | Command | Script (pinned) |
|---|---|---|
| Deploy | `./deploy-fork.sh` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) |
| Same refresh period on both hubs (previously 30s and 180s) | `./reconcile.sh 60s` | [`reconcile.sh` @ be596a122](https://github.com/ImIOImI/argo-cd/blob/be596a122/hack/multi-hub-lab/reconcile.sh) |
| 10 extra identical apps (`lab-1`…`lab-10`) on both hubs | `./apps.sh 10` | [`apps.sh` @ be596a122](https://github.com/ImIOImI/argo-cd/blob/be596a122/hack/multi-hub-lab/apps.sh) |
| **ON**: sharing on (default), 10 min | `SINCE=15m ./refresh-counts.sh <from> <to>` | [`refresh-counts.sh` @ be596a122](https://github.com/ImIOImI/argo-cd/blob/be596a122/hack/multi-hub-lab/refresh-counts.sh) |
| **OFF**: sharing off on both hubs, 60s to settle, then 9m40s | `./multihub-env.sh ARGOCD_MULTIHUB_SHARE_REFRESH=false`, then as above | [`multihub-env.sh` @ e028e0111](https://github.com/ImIOImI/argo-cd/blob/e028e0111/hack/multi-hub-lab/multihub-env.sh) |
| **Failover**: sharing back on, 90s, stop hub-b; count the first minute, then 9 min | `./multihub-env.sh ARGOCD_MULTIHUB_SHARE_REFRESH-`, `./controller.sh hub-b stop`, as above | [`controller.sh` @ fcc7bafb4](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/controller.sh) |
| **Sync check**: hub-b back, one commit, count automated syncs per app over 5 min | `./controller.sh hub-b start; ./bump.sh`, then `kubectl logs` on both controllers filtered to `Initiated automated sync` (one-off command; its output is `sync-check.txt`) | [`bump.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/bump.sh) |

The waits between windows were `sleep` or an until-loop on the clock.

Raw data: [data/2026-11-21-c7-shared-refresh](data/2026-11-21-c7-shared-refresh)
(`on.txt`, `off.txt`, `failover.txt`, `sync-check.txt`).

---

## R1. Work per hub roughly halves

```
=== E11 ON  18:28:35 -> 18:38:35 (sharing on)
hub-a: full=34 shared=30 apps=11
hub-b: full=38 shared=25 apps=11
=== E11 OFF 18:40:24 -> 18:50:04 (sharing off)
hub-a: full=61 shared=0 apps=11
hub-b: full=63 shared=0 apps=11
```

**Why 56% and not 50%:** a hub refreshes for itself when no fresh record
exists, and that happens in two situations:
- **Both hubs come due within moments of each other,** before either
  record has been published or watched.
- **The other hub's record is older than one period,** because its own
  timer plus jitter ran long (the jittered period here averaged about
  100s, against the 60s freshness limit).

Both are consequences of a strict "fresh within one period" rule. They
waste some work but never cost correctness. If the remaining ~12% matters,
a longer freshness limit (about 1.5 periods) would cut it, at the cost of
results up to 50% older on the skipping hub. That's a tuning knob; I didn't
build it.

## R2. Failover

```
=== E11 KILL first minute 18:52:30 -> 18:53:30 (hub-b stopped at 18:52:30)
hub-a: full=3 shared=0
=== E11 KILL rest 18:53:30 -> 19:02:30
hub-a: full=63 shared=0 apps=11
```

hub-a skipped nothing from the moment hub-b stopped. hub-b's last records
were at most a period old and became stale within the minute. hub-a's rate
over the next 9 minutes (63, i.e. 70 per 10 min) matches the unshared
control.

## R3. Syncing still happens exactly once

After one commit with both hubs up and sharing on:

```
hub-a automated syncs: 4 apps (lab-1, lab-10, lab-3, lab-9)
hub-b automated syncs: 7 apps (lab-2, lab-4, lab-5, lab-6, lab-7, lab-8, lab-app)
all 11 apps Synced on both hubs; every spoke ConfigMap at generation 21
```

Each app was synced once, by whichever hub's refresh found the new commit
first. Upstream logs `Initiated automated sync` twice per sync, so the raw
counts in `sync-check.txt` are doubled; lab-1's log shows two identical
lines at 19:07:20 followed by one `Initialized new operation`. The other hub
saw the app become Synced through the next shared or own refresh.

---

## Known gaps

1. **Event-driven refreshes aren't shared.** Both hubs still react to every
   resource change on the spoke with their own comparison. In the lab these
   are rare. On busy clusters they may be a large part of the work, and
   that's worth measuring before deciding to share them too.
2. **The skipping hub's per-resource diffs** (the UI's Diff view) are as of
   its own last full refresh, at most about two periods old. Status, health
   and tree are current.
3. **Comparison conditions aren't copied.** Results are only published from
   comparisons without error conditions. Warnings, such as a shared-resource
   warning, aren't carried over to the skipping hub.
4. **Multi-source, Helm and OCI apps** don't share, because there's no pin
   to agree on.
5. **RBAC outside the lab.** On the spoke, the hubs now also need
   `list`/`watch` on Leases and write access to ConfigMaps in the
   coordination namespace. The lab uses cluster-admin.
6. **The extra apps make the lab slower:** every commit now syncs 11 apps.
   `./apps.sh 0` removes them.
