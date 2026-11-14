# 2026-11-14: C2 telemetry (logs, Events, metrics) and their settings

**Question:** can operators see what the operation lease is doing, and turn
the noise up or down, without losing the signals that matter?

**Short answer:** yes, with two live runs.
- With the defaults, every lease decision is one structured log line.
  Repeated refusals collapse to one info line per change. A user operation
  waiting on another hub gets one Kubernetes Event. A counter tracks every
  decision.
- Switching the settings to quiet removes all routine info lines and Events
  while the metrics keep counting. Switching them to verbose adds per-renewal
  lines.
- The run also exercised, for the first time live, a **manual sync waiting
  behind another hub's lease**. It waited ~14s, then ran.

Follows up the gaps listed at the end of the
[C2 lease note](2026-11-14-c2-operation-lease.md).

## What changed

Commit [`e028e0111`](https://github.com/ImIOImI/argo-cd/commit/e028e0111).
Full reference:
[`controller/multihub/README.md`](https://github.com/ImIOImI/argo-cd/blob/e028e0111/controller/multihub/README.md).

| Gap (from the C2 note) | Now |
|---|---|
| Renewals and the operation path's acquire/takeover were silent | Every decision on both paths is logged (renewals at debug by default) |
| Releases weren't logged | `released` at info |
| Takeover was info | `takeover` and `lost` are **warn**, always, plus a Warning Event |
| Refusals logged on every refresh (4×/s seen in E7) | Collapsed per app + code path: info when the event or holder changes, debug for repeats |
| Lease details only in the message text | Fields: `multihub.hub/path/event/holder/generation/reason/retry_after` |
| No metrics | `argocd_multihub_lease_decisions_total{hub,path,event}` on the controller's `/metrics` |
| No Kubernetes Events | `MultihubLeaseTakeover`, `MultihubLeaseLost`, `MultihubLeaseError`, `MultihubLeaseReleaseError` (Warning), `MultihubOperationWaiting` (Normal, once per wait) |

**Settings** (env on the application controller; defaults in brackets):
`ARGOCD_MULTIHUB_LOG_LIFECYCLE` [true], `_LOG_RENEWALS` [false],
`_LOG_DENIALS` [changes | all | off], `_EVENTS` [true], `_METRICS` [true].
Two design rules:
1. Anything switched off is demoted to **debug**, never dropped.
2. Warnings (takeover, lost lease, lease errors) **can't be switched off**.
   Errors are collapsed like refusals but always warn when they change.

The startup line logs the effective settings as fields.

Design choices:
- The metric is registered in controller-runtime's registry, which
  `/metrics` already serves, so upstream's `controller/metrics/metrics.go`
  is untouched.
- No per-app metric labels, to keep cardinality bounded. Per-app detail is in
  the logs and Events.

Tests: 12 new in `controller/multihub` (reporting policy, env parsing,
invalid values, idempotent metric registration, the event type on every
lease decision). Total with C2: 22. The existing `./controller/` suite
passes unchanged (91s).

## How it was run

Scripts are in `hack/multi-hub-lab/`, run from that directory; same
environment as before.

| Step | Command | Script (pinned) |
|---|---|---|
| Tests | `go test ./controller/multihub/ ./controller/ -count=1` (repo root) | code @ [`e028e0111`](https://github.com/ImIOImI/argo-cd/tree/e028e0111/controller/multihub) |
| Deploy (multihub on, default telemetry) → `argocd-multihub:e028e0111` | `./deploy-fork.sh` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) |
| Warm both repo caches after the redeploy | `./refresh-both.sh && ./wait-synced.sh`, then wait for hub-b's refresh to finish | [`refresh-both.sh` @ 36e7e2c79](https://github.com/ImIOImI/argo-cd/blob/36e7e2c79/hack/multi-hub-lab/refresh-both.sh), [`wait-synced.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/wait-synced.sh), [`wait-refreshed.sh` @ d0811ebad](https://github.com/ImIOImI/argo-cd/blob/d0811ebad/hack/multi-hub-lab/wait-refreshed.sh) |
| **V1** (defaults): race + manual sync queued behind hub-a | `./bump.sh && ./refresh-both.sh && ./wait-lease.sh hub-a && ./manual-sync.sh hub-b` (16:27:00Z, `167d509`) | [`bump.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/bump.sh), [`wait-lease.sh` @ ba8a0ace9](https://github.com/ImIOImI/argo-cd/blob/ba8a0ace9/hack/multi-hub-lab/wait-lease.sh), [`manual-sync.sh` @ e028e0111](https://github.com/ImIOImI/argo-cd/blob/e028e0111/hack/multi-hub-lab/manual-sync.sh) |
| Apply **V2** settings (restarts controllers only) | `HUBS=hub-b ./multihub-env.sh ARGOCD_MULTIHUB_LOG_DENIALS=off ARGOCD_MULTIHUB_LOG_LIFECYCLE=false ARGOCD_MULTIHUB_EVENTS=false`<br>`HUBS=hub-a ./multihub-env.sh ARGOCD_MULTIHUB_LOG_DENIALS=all ARGOCD_MULTIHUB_LOG_RENEWALS=true` | [`multihub-env.sh` @ e028e0111](https://github.com/ImIOImI/argo-cd/blob/e028e0111/hack/multi-hub-lab/multihub-env.sh) |
| Confirm effective settings | `./config.sh` | [`config.sh` @ fad325935](https://github.com/ImIOImI/argo-cd/blob/fad325935/hack/multi-hub-lab/config.sh) |
| **V2** (toggled): same flow | as V1 (16:30:06Z, `47b03d6`) | as V1 |
| Lease log lines | `SINCE=<w> AFTER=<t> [RAW=1] ./lease-log.sh` | [`lease-log.sh` @ 28ef6baba](https://github.com/ImIOImI/argo-cd/blob/28ef6baba/hack/multi-hub-lab/lease-log.sh) |
| Events | `./events.sh` | [`events.sh` @ e028e0111](https://github.com/ImIOImI/argo-cd/blob/e028e0111/hack/multi-hub-lab/events.sh) |
| Metrics | `./metrics.sh` | [`metrics.sh` @ 33070bb36](https://github.com/ImIOImI/argo-cd/blob/33070bb36/hack/multi-hub-lab/metrics.sh) |
| Sync timeline | `SINCE=<w> AFTER=<t> ./syncs.sh` | [`syncs.sh` @ 40529be4d](https://github.com/ImIOImI/argo-cd/blob/40529be4d/hack/multi-hub-lab/syncs.sh) |
| Raw data | `SINCE=<w> ./collect.sh <dir>` | [`collect.sh` @ 07e28cdae](https://github.com/ImIOImI/argo-cd/blob/07e28cdae/hack/multi-hub-lab/collect.sh) |

**Provenance and tooling fixes made during the run:**
- `config.sh` and `wait-refreshed.sh` were first run as one-off commands
  (reading the startup line; waiting for hub-b's refresh annotation to
  clear). They were saved afterwards (d0811ebad) and re-run. The first saved
  `config.sh` printed a false "(multihub not enabled)" after each match
  (`pipefail` + `grep -m1`); fixed in fad325935 and re-run with correct
  output.
- `syncs.sh` @ c09328a9f assumed every operation has a revision and printed
  the manual sync as a raw JSON line. It was fixed (40529be4d) before
  `syncs.txt` was saved.
- `metrics.sh` @ e028e0111 printed correctly but exited 143 (the status of
  the port-forward it kills), which cut a `&&` chain short. Fixed in
  33070bb36. V1's `metrics.txt` came from the first version (same output),
  V2's from the fix.
- All other scripts were committed before they ran.

Raw data: [data/2026-11-14-c2-telemetry](data/2026-11-14-c2-telemetry)
(`v1-defaults/`, `v2-toggled/`: `lease-log.txt`/`.jsonl`, `events.txt`,
`metrics.txt`, `syncs.txt`, plus the usual `collect.sh` files).

---

## R1 (V1). Defaults

Lease log (`lease-log.txt`: time, level, event, path, holder, generation, message):
```
== hub-a
16:27:01 info  acquired  auto-sync hub-a  13  multihub: operation lease acquired (generation 13)
16:27:16 info  released  operation hub-a  -   multihub: released operation lease
== hub-b
16:27:01 info  held      auto-sync hub-a  -   Skipping auto-sync: operation lease held by hub-a (rechecking in 15s)
16:27:02 info  held      operation hub-a  -   Waiting to start operation: operation lease held by hub-a
16:27:16 info  acquired  operation hub-b  14  multihub: operation lease acquired (generation 14)
16:27:32 info  released  operation hub-b  -   multihub: released operation lease
```
Metrics (`metrics.txt`, hub-b):
```
argocd_multihub_lease_decisions_total{event="held",hub="hub-b",path="auto-sync"} 2
argocd_multihub_lease_decisions_total{event="held",hub="hub-b",path="operation"} 3
argocd_multihub_lease_decisions_total{event="renewed",hub="hub-b",path="operation"} 3
...
```
Events (`events.txt`, hub-b):
`Normal  MultihubOperationWaiting  count=1  Waiting to start operation: operation lease held by hub-a`

- **Dedup works:** hub-b was refused 5 times (2 auto-sync, 3 polls of the
  waiting operation), which is **2 info lines**, one per code path. The metric
  shows the true count.
- **Renewals** (hub-a 5, hub-b 3) were counted but not logged at info, as
  configured.
- **One Event for the whole wait**, not one per 5s poll.
- **Manual sync waiting on the lease, now tested live:** hub-b's manual sync
  (16:27:02) waited until hub-a released at 16:27:16. hub-b then took the
  lease (generation 14), ran (`rev=(target)` in `syncs.txt`) and released.
  This was unit-test-only in the C2 note.

## R2 (V2). Settings switched (hub-b quiet, hub-a verbose)

`./config.sh` confirmed both hubs' settings from their startup lines
(`log_denials: off / all`, etc.).

```
== hub-a
16:30:08 info  acquired  auto-sync hub-a  15  multihub: operation lease acquired (generation 15)
16:30:08 info  renewed   operation hub-a  15  multihub: operation lease renewed (generation 15)
16:30:10 info  renewed   operation hub-a  15  ...  (4 renewals in total)
16:30:21 info  released  operation hub-a  -   multihub: released operation lease
== hub-b
(nothing)
```
- **hub-b logged no lease lines at info**, although its metrics show 2+2
  refusals, an acquire, 4 renewals and a release, and its manual sync waited
  and ran as in V1. No new Event: the only one listed is V1's at 16:27:02.
  Metrics stayed on.
- **hub-a logged every renewal at info** (`LOG_RENEWALS=true`).

## Not verified live

- **`LOG_DENIALS=all`:** hub-a was set to it, but in V2 hub-a was never
  refused.
- **Demotion to debug:** the controllers run at `--loglevel info`, so the
  debug lines weren't visible. The levels are unit-tested (`Plan`).
- **Warning paths** (takeover, lost lease, lease errors, and their Events):
  the policy is unit-tested, but nothing has triggered them live yet. A
  takeover needs a hub that stops renewing for >60s mid-operation (scale
  its controller to 0), which belongs with the takeover tests (C3).

## Lab state afterwards

Both hubs run `argocd-multihub:e028e0111` with multihub on. hub-a:
`LOG_DENIALS=all, LOG_RENEWALS=true`. hub-b: `LOG_DENIALS=off,
LOG_LIFECYCLE=false, EVENTS=false`. Reset with
`./multihub-env.sh ARGOCD_MULTIHUB_LOG_DENIALS- ARGOCD_MULTIHUB_LOG_RENEWALS- ARGOCD_MULTIHUB_LOG_LIFECYCLE- ARGOCD_MULTIHUB_EVENTS-`.
