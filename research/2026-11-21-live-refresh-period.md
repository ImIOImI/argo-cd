# One line in Git sets both hubs' refresh period, with no restart

**Date:** 2026-11-21 · **Fork commits:** `0e530d2b3` → `7ca88d158`, `45f109164` → `dbb753298`
· **Lab image:** `argocd-multihub:88b225f49`

## Why

Troy wants to control how often both hubs refresh, with as simple a change
as possible. Upstream, the period (`timeout.reconciliation` in `argocd-cm`)
is read once, when the controller starts. Changing it means editing each
hub's `argocd-cm` and restarting each controller.

A restart has real costs with two hubs:
- the controller rebuilds its watch of every spoke from scratch;
- if both hubs restart together, neither is reconciling for a while.

## Change

**Fork.** With multihub on, the controller reads `timeout.reconciliation`
from `argocd-cm` **live**, using the settings manager's existing in-memory
cache, so there's no API call. Where the period is used:
- the expiry check;
- the shared-refresh (C7) freshness window;
- a **re-check loop** that queues every app once per *current* period. The
  app informer's own resync timer is fixed at startup, so a lowered period
  would otherwise only take effect at the old pace.

A missing or invalid value, or multihub off, keeps the startup value.

**Lab bootstrap.** A self-managing root app on each hub
(`bootstrap/root.yaml`, applied once with `root.sh`) syncs `bootstrap/`.
That directory holds an `argocd-cm` manifest with **only**
`timeout.reconciliation`, applied server-side:
- Argo owns just that key, and each hub keeps its own `url`, SSO and so on.
- It's marked `Prune=false,Delete=false`, so removing the root can never
  delete `argocd-cm`.

## TDD record

| Red | Green | What | Red failures |
|---|---|---|---|
| `0e530d2b3` | `7ca88d158` | `refreshPeriod()` follows argocd-cm live; expiry and C7 use it | 4 (incl. a subtest; the "2m" subtest is a guard that passes at red) |
| `45f109164` | `dbb753298` | resync loop on the live period, re-read every round | 1 |

**Not covered by tests:** the loop's handling of a `0` (disabled) period,
and its jitter path. That path does the same as upstream's informer resync.

`go test ./controller/...` passes at `dbb753298`.

## How it was run

Lab scripts are in `hack/multi-hub-lab/` and were run from that directory.
Both hubs were on 60s (from `reconcile.sh`), with 11 apps.

| Step | Command | Script (pinned) |
|---|---|---|
| Deploy | `./deploy-fork.sh` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) |
| Give each hub its own `url` (to show it's left alone) | `kubectl patch cm argocd-cm` on each hub with `url: https://<hub>.lab.example` (one-off command) | n/a |
| Bootstrap the root on both hubs | `./root.sh` | [`root.sh`](https://github.com/ImIOImI/argo-cd/blob/88b225f49/hack/multi-hub-lab/root.sh), [`bootstrap/`](https://github.com/ImIOImI/argo-cd/tree/88b225f49/hack/multi-hub-lab/bootstrap) @ 88b225f49 |
| Show each hub's value, its owner and `url` | `./period.sh` | [`period.sh` @ 88b225f49](https://github.com/ImIOImI/argo-cd/blob/88b225f49/hack/multi-hub-lab/period.sh) |
| **The one-line change** 60s → 120s, pushed (commit 2976b9729) | `./set-period.sh 120s` | [`set-period.sh` @ 88b225f49](https://github.com/ImIOImI/argo-cd/blob/88b225f49/hack/multi-hub-lab/set-period.sh) |
| Controller pod start times and restart counts, before and after | `kubectl get pod argocd-application-controller-0 -o jsonpath=…` (one-off command) | n/a |
| Refreshes over 10 min, starting 60s after both hubs showed 120s | `SINCE=15m ./refresh-counts.sh <from> <to>` | [`refresh-counts.sh` @ be596a122](https://github.com/ImIOImI/argo-cd/blob/be596a122/hack/multi-hub-lab/refresh-counts.sh) |

Raw data: [data/2026-11-21-live-refresh-period](data/2026-11-21-live-refresh-period).

---

## R1. Bootstrapping keeps each hub's own settings

After `./root.sh`, both roots were Synced and Succeeded:

```
hub-a timeout.reconciliation=60s owned-by=argocd-controller,kubectl-patch url=https://hub-a.lab.example
hub-b timeout.reconciliation=60s owned-by=argocd-controller,kubectl-patch url=https://hub-b.lab.example
```

Argo took ownership of the one key alongside the earlier manager. The
values matched, so there was no conflict. Each hub's `url` was untouched.

## R2. The change reaches both hubs, with no restart

```
pushed refresh period 120s at 22:27:55Z
22:29:31Z
hub-a timeout.reconciliation=120s owned-by=argocd-controller url=https://hub-a.lab.example
hub-b timeout.reconciliation=120s owned-by=argocd-controller url=https://hub-b.lab.example
hub-a started=2026-11-21T22:23:49Z restarts=0   (same before and after)
hub-b started=2026-11-21T22:25:24Z restarts=0   (same before and after)
```

- **96 seconds** from push to both hubs holding 120s. That's the roots'
  60s poll plus jitter; a Git webhook to both hubs would make it seconds.
- Argo is now the **sole owner** of the key, having taken it over from the
  manual patch.
- **No controller restarted.**

## R3. Both controllers actually use it

```
=== period 120s (set from Git, no restart) 22:30:38 -> 22:40:40
hub-a: full=29 shared=20 apps=12
hub-b: full=22 shared=25 apps=12
```

| | 60s (C7 note, sharing on) | 120s (live) |
|---|---|---|
| hub-a came due (full + shared) | 64 | 49 |
| hub-b came due | 63 | 47 |
| Some hub fully refreshed each app | ~every 92s | ~every 141s |
| Shared (skipped) share of each hub's refreshes | 47% / 40% | 41% / 53% |

The 120s window counts 12 apps, because the root app's name matches the
`lab-` filter in `refresh-counts.sh`.

- **Both controllers still log their startup period**
  (`appResyncPeriod=1m0s, … appResyncJitter=1m0s`, in `hub-*-startup.jsonl`).
  They slowed down anyway, so the live value is what's in force.
- **Why not exactly half:** the controller delays each periodic re-check by
  a random amount up to the jitter (60s here, about 30s on average) on top
  of the period. 60+30 ≈ 90s and 120+30 ≈ 150s, which matches the measured
  92s and 141s. The jitter is still read only at startup.
- **Shared refresh kept working** at the new period. It skips if the other
  hub refreshed within *this* hub's current period.

## Overhead

Not benchmarked; this is what the code does.
- **Each "is it due?" check now also reads `argocd-cm`** from the settings
  manager's cache: a mutex, a map lookup, a deep copy of the ConfigMap and
  a duration parse. That's microseconds and a few KB of short-lived
  allocation, against a refresh measured at 50–400 ms in this lab.
- **The re-check loop duplicates the app informer's own (now redundant)
  resync,** so each app gets about two cheap due-checks per period instead
  of one.
- **Both costs can be removed if they ever matter:**
  - parse the period once per `argocd-cm` change, via the settings
    manager's notification;
  - turn off the informer resync when multihub is on.

## Known gaps

1. **An invalid value is silently ignored.** The controller stays on its
   startup value with no log or Event.
2. **No log line or metric** shows the period in force or when it changed.
3. **`timeout.reconciliation.jitter` is still startup-only.**
4. **A per-hub period can't be set by editing one hub.** The root's
   self-heal reverts it. That's deliberate: equal periods keep shared
   refresh balanced.
5. **The lab's earlier `reconcile.sh` (which restarts controllers)** is now
   superseded for this setting by `set-period.sh`.
