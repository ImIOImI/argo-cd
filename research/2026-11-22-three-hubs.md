# Three hubs: coordination holds, but refresh sharing doesn't balance

**Date:** 2026-11-22 · **Lab images:** `argocd-multihub:01750a440` (all three hubs)
· **No new fork code:** this note verifies what exists with a third hub.

## Summary

hub-c was added to the lab, with the same fork, the same root app (120s
refresh period from Git) and the same 12 apps.

| Experiment | Result with three hubs |
|---|---|
| **E7**: all hubs discover a commit together | **1 operation and 1 hook Job per app** (11 and 11), every app Synced on all three |
| **E8**: hub-a dies mid-hook holding all 11 syncs | **each sync claimed by exactly one survivor** (hub-b 8, hub-c 3; every Lease generation +1), **1 hook Job per app**, all Synced. hub-a came back and followed all 11 (0 stale resumes). |
| **E11**: refresh sharing | **Total work right, distribution wrong:** together the three did about one hub's worth, but hub-a did about 80% of it. With hub-c stopped, the remaining two split more evenly. |

Everything coordinated through a single compare-and-swap on the Lease works
unchanged with N hubs:
- the lock (C2);
- the pin (C1);
- takeover (C3), including two survivors racing to claim the same sync;
- following the result when a hub returns.

**Shared refresh (C7) needs a balancing rule.** Its "skip if another hub just
refreshed" lets whichever hub leads keep the work, and with three hubs the
refresh jitter didn't reshuffle it (R3).

## How it was run

Lab scripts are in `hack/multi-hub-lab/` and were run from that directory.

| Step | Command | Script (pinned) |
|---|---|---|
| Scripts loop over any number of hubs | `./lab-hubs.sh` (every kind cluster named `hub-*`) | [`lab-hubs.sh` @ 01750a440](https://github.com/ImIOImI/argo-cd/blob/01750a440/hack/multi-hub-lab/lab-hubs.sh) |
| Add hub-c (stock Argo, spoke, lab-app) | `./add-hub.sh hub-c` | [`add-hub.sh` @ 01750a440](https://github.com/ImIOImI/argo-cd/blob/01750a440/hack/multi-hub-lab/add-hub.sh) |
| Fork on all three; root app on hub-c; 10 extra apps on hub-c | `./deploy-fork.sh`, `kubectl patch cm argocd-cm` (hub-c `url`, one-off), `HUBS=hub-c ./root.sh`, `./apps.sh 10` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) (script content; the hub loop came from `lab-hubs.sh` at 01750a440) |
| Warm caches; all 12 apps Synced and Healthy on all three hubs | `./refresh-both.sh && ./wait-refreshed.sh`, then a polling loop | [`refresh-both.sh` @ 01750a440](https://github.com/ImIOImI/argo-cd/blob/01750a440/hack/multi-hub-lab/refresh-both.sh) |
| **E7** | `./bump.sh && ./refresh-both.sh` (lab-app refreshed on all three; the rest on their own timers), then 5.5 min later `SINCE=15m ./ops-count.sh <start>` | [`ops-count.sh` @ 40cf8a79f](https://github.com/ImIOImI/argo-cd/blob/40cf8a79f/hack/multi-hub-lab/ops-count.sh) |
| **E8** | `./hook-sleep.sh 120; ./bump.sh`; hard refresh of every lab app on hub-a (one-off `kubectl annotate` loop); `./wait-lease.sh hub-a && ./wait-hook.sh && sleep 10 && ./controller.sh hub-a stop`; after 5 min: Lease holders and generations, takeover lines from hub-b's and hub-c's logs, `./ops-count.sh` | [`wait-hook.sh`](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/wait-hook.sh), [`controller.sh`](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/controller.sh) @ fcc7bafb4 |
| hub-a returns | `./controller.sh hub-a start; sleep 40`; count `following` and `Resuming in-progress operation` lines | as above |
| **E11** | hook back to 5s; 60s settle; 10 min counted; `./controller.sh hub-c stop`; 2 min settle; 10 min counted | [`refresh-counts.sh` @ be596a122](https://github.com/ImIOImI/argo-cd/blob/be596a122/hack/multi-hub-lab/refresh-counts.sh) |

Raw data: [data/2026-11-22-three-hubs](data/2026-11-22-three-hubs).

**Notes:**
- `ops-count.sh` counts operations from controller logs. A stopped hub has
  no logs, so in E8 the "operations=0" lines don't show hub-a's original
  operations. Hook Jobs, Lease generations and takeover lines are the
  evidence there.
- The scripts named `-both` predate hub-c; they now act on every hub.

---

## R1. E7: a commit discovered by three hubs at once

```
hub-a: operations=2 {lab-4, lab-9}
hub-b: operations=2 {lab-2, lab-app}
hub-c: operations=7 {lab-1, lab-10, lab-3, lab-5, lab-6, lab-7, lab-8}
hook jobs created: 11 in 11 namespaces (1 each)
hub-a/b/c: 12 Synced/Healthy each; every spoke ConfigMap at generation 22
```

There were no duplicate operations and no duplicate hooks. Which hub ran
each app's sync is just whoever's refresh found the commit first.

## R2. E8: two survivors, eleven orphans

At 03:05:26 hub-a was stopped. It held all 11 leases (generation 4, and 43
for lab-app), with 120s hooks running. After 5 minutes:

```
hub-b takeovers: lab-10 lab-1 lab-3 (03:06:10) lab-4 lab-5 (03:06:14) lab-8 lab-9 (03:06:17) lab-app (03:06:19)
hub-c takeovers: lab-2 (03:06:09) lab-6 (03:06:16) lab-7 (03:06:17)
every Lease at generation 5 (lab-app 44), no holder: each claimed exactly once
hook jobs created: 11 in 11 namespaces (1 each): hub-a's running hooks were adopted
hub-b, hub-c: 12 Synced/Healthy
```

When hub-a returned, it logged `following` for all 11 apps, resumed none of
its stale operations, created no new hook Jobs, and showed 12 apps
Synced/Healthy.

The unit test `TestClaimRaceHasOneWinner` predicted this. It's now
confirmed live: 11 claims spread across two racing hubs, and not one
double claim.

## R3. E11: the total is right, the distribution isn't

```
=== three hubs, 10 min, 120s period
hub-a: full=43 shared=7    (own root app: 4 full)
hub-b: full=12 shared=35   (own root: 4)
hub-c: full=7  shared=31   (own root: 3)
=== hub-c stopped, next 10 min (after 2 min settling)
hub-a: full=26 shared=12
hub-b: full=19 shared=17
```

Each hub's root app deploys to that hub's own cluster, so it's never
shared. Counting only the 11 shared apps:

| | hub-a | hub-b | hub-c | Total full | Some hub fully refreshed each app |
|---|---|---|---|---|---|
| Three hubs | 39 (76%) | 8 | 4 | 51 | ~every 129s |
| hub-c stopped | ~22 | ~15 | n/a | ~37 | ~every 178s |

The hub-c-stopped root counts weren't split out, so its figures are
approximate (about 4 root refreshes subtracted per hub).

- **No work is wasted.** The total, 51 full refreshes in 10 minutes for 11
  apps on a 120s period, is about one hub's worth, as intended.
- **But one hub carries it.** Why:
  - **The skip rule makes the lead sticky.** A hub skips if *any* other hub
    refreshed within its own period, and a hub that skips pushes its next
    attempt a full period out. So whichever hub comes due first keeps coming
    due first, and the others keep skipping.
  - **The lead came from a restart.** hub-a had restarted two minutes before
    the window (after E8), which gave it fresh timers.
  - **With two hubs this didn't show.** In the C7 note, Argo's refresh
    jitter reshuffled the order often enough that each app alternated. With
    three hubs, that didn't happen here.
- **Failover still works**, and with two hubs left they split it more
  evenly.

---

## What this means

**Coordination and takeover are ready for three or more hubs.**

**Shared refresh needs a balancing rule** before "each hub does its share"
holds for N > 2. The natural fix uses the hub list (`multihub.hubs`) decided
for C8 and C9:
- **Pick a preferred hub per app and period.** Rotate it deterministically,
  e.g. a hash of (app, period number, hub) over the listed hubs that are
  live. Every hub computes the same answer, with no messages.
- **The preferred hub refreshes when due. The others skip** while its
  result is fresh, and take over only if it's late (more than about 1.5
  periods). That covers a dead or slow hub.

That gives an even split, per-app rotation and automatic failover, and it
doesn't depend on jitter. It needs the hub list first.

## Known gaps

1. Balanced sharing for N > 2 (above).
2. The hub list (`multihub.hubs`) doesn't exist yet; C8, C9 and balancing
   all need it.
3. `observe.sh` is still two-hub (baseline only).
