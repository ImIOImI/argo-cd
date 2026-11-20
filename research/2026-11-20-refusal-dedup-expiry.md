# Refusal-log dedup now expires after a quiet gap

**Date:** 2026-11-20 · **Fork commits:** `5671e59ab` → `24280921e`,
`f80e30311` → `c67b396e7` · **Lab image:** `argocd-multihub:c67b396e7`

## Why

In the C1 note's E7 run ([2026-11-14-c1-revision-pin.md](2026-11-14-c1-revision-pin.md)),
the metric showed hub-b being refused the lease 4 times, but none of those
refusals reached the info log. With `ARGOCD_MULTIHUB_LOG_DENIALS=changes`, the
reporter remembered the last refusal per (app, path) and collapsed identical
repeats to debug. That memory was cleared only when the hub itself acquired
the lease. hub-b's earlier refusal ("held by hub-a", 7 minutes before) was
therefore still remembered, and the new episode looked like a repeat.

The behavior was right, but an operator watching the info log couldn't see it.

## Change

A new setting, `ARGOCD_MULTIHUB_LOG_DENIALS_RESET_AFTER` (default `2m`, `0`
means never), controls the dedup:

- A repeat refusal or error is reported again once none has been seen for that
  long.
- The gap is measured from the **last one seen**, not the last one logged, so
  a continuous episode (rechecks every ≤15s) stays collapsed however long it
  lasts.
- It applies to lease refusals (`held`, `settling`), lease errors and pin
  errors. A waiting manual operation also gets one new Kubernetes Event per
  episode.
- The startup line (`multihub coordination enabled`) now includes
  `multihub.log_denials_reset_after`, so `config.sh` can confirm the setting.

The design is in `Reporter.changed` in
[`controller/multihub/telemetry.go`](https://github.com/ImIOImI/argo-cd/blob/c67b396e7/controller/multihub/telemetry.go).

## TDD record

| Pair | Red | Green | Proof |
|---|---|---|---|
| Dedup expiry | `5671e59ab` (stubs: setting name, `Telemetry.DenialsResetAfter`, injectable clock) | `24280921e` | `red-green.sh 5671e59ab HEAD ./controller/multihub/`: RED fails `TestDenialsRepeatIsNewsAfterQuietGap`, `TestErrorsWarnAgainAfterQuietGap`, `TestOperationWaitEventRepeatsAfterQuietGap`, `TestTelemetryFromEnvDefaults`, `TestTelemetryFromEnvOverrides`, `TestTelemetryFromEnvResetAfter`; GREEN ok; PROVEN |
| Startup line | `f80e30311` (no stubs needed) | `c67b396e7` | `red-green.sh f80e30311 HEAD ./controller/ '^TestMultihub'`: RED fails `TestMultihubInitLogsEverySetting`; GREEN ok; PROVEN |

`TestResetAfterZeroNeverForgets` passes at red on purpose. It locks in the old
behavior for `0`.

The second pair came from the live check. The first deployment's `config.sh`
output didn't show the new setting, so it got its own red test before the
redeploy.

## How it was run

Lab scripts are in `hack/multi-hub-lab/` and were run from that directory, on
the same kind lab (hub-a polls Git every 30s, hub-b every 180s). Telemetry
settings were at their defaults.

| Step | Command | Script (pinned) |
|---|---|---|
| Deploy → `argocd-multihub:c67b396e7` | `./deploy-fork.sh` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh) |
| Confirm settings (`2m0s` on both hubs) | `./config.sh` | [`config.sh` @ fad325935](https://github.com/ImIOImI/argo-cd/blob/fad325935/hack/multi-hub-lab/config.sh) |
| Warm both repo caches | `./refresh-both.sh && ./wait-refreshed.sh && ./wait-synced.sh` | [`refresh-both.sh` @ 36e7e2c79](https://github.com/ImIOImI/argo-cd/blob/36e7e2c79/hack/multi-hub-lab/refresh-both.sh), [`wait-refreshed.sh` @ d0811ebad](https://github.com/ImIOImI/argo-cd/blob/d0811ebad/hack/multi-hub-lab/wait-refreshed.sh), [`wait-synced.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/wait-synced.sh) |
| Three E7 episodes, each after ≥3 min of quiet | `./bump.sh && ./refresh-both.sh` at 12:12:06Z, 12:16:22Z, 12:20:57Z | [`bump.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/bump.sh) |
| Lease log | `SINCE=15m AFTER=2026-11-20T12:12:06 ./lease-log.sh` | [`lease-log.sh` @ 28ef6baba](https://github.com/ImIOImI/argo-cd/blob/28ef6baba/hack/multi-hub-lab/lease-log.sh) |
| Syncs | `SINCE=15m AFTER=2026-11-20T12:12:06 ./syncs.sh` | [`syncs.sh` @ ca2b48900](https://github.com/ImIOImI/argo-cd/blob/ca2b48900/hack/multi-hub-lab/syncs.sh) |
| Metrics | `./metrics.sh` | [`metrics.sh` @ 33070bb36](https://github.com/ImIOImI/argo-cd/blob/33070bb36/hack/multi-hub-lab/metrics.sh) |
| Raw data | `SINCE=15m ./collect.sh <dir>` | [`collect.sh` @ 07e28cdae](https://github.com/ImIOImI/argo-cd/blob/07e28cdae/hack/multi-hub-lab/collect.sh) |

The waits between episodes were plain `sleep`-style pauses in the shell, not a
script.

Raw data: [data/2026-11-20-refusal-dedup-expiry](data/2026-11-20-refusal-dedup-expiry).

## Result

`lease-log.txt` (info level; debug isn't shown at the lab's log level):

```
== hub-a
12:12:10 info  advanced  pin       -      -   multihub: advanced the pinned revision to 22fcada7…
12:12:10 info  acquired  auto-sync hub-a  19  multihub: operation lease acquired (generation 19)
12:12:29 info  released  operation hub-a  -   multihub: released operation lease
12:16:26 info  held      auto-sync hub-b  -   Skipping auto-sync: operation lease held by hub-b (rechecking in 15s)
12:21:02 info  held      auto-sync hub-b  -   Skipping auto-sync: operation lease held by hub-b (rechecking in 15s)
== hub-b
12:12:11 info  held      auto-sync hub-a  -   Skipping auto-sync: operation lease held by hub-a (rechecking in 15s)
12:16:26 info  advanced  pin       -      -   multihub: advanced the pinned revision to f4c8cca3…
12:16:26 info  acquired  auto-sync hub-b  20  multihub: operation lease acquired (generation 20)
12:16:41 info  released  operation hub-b  -   multihub: released operation lease
12:21:02 info  advanced  pin       -      -   multihub: advanced the pinned revision to 217858e7…
12:21:02 info  acquired  auto-sync hub-b  21  multihub: operation lease acquired (generation 21)
12:21:20 info  released  operation hub-b  -   multihub: released operation lease
```

- **The fix, observed:** hub-a's refusal at **12:21:02** is word for word the
  same as its refusal at 12:16:26, 4½ minutes earlier, and hub-a didn't
  acquire the lease in between. Before this change it would have gone to
  debug. It's now at info.
- **Repeats within an episode still collapse.** `metrics.txt` counts
  `held` = 9 on hub-a and 4 on hub-b, against 2 and 1 info lines. Each
  episode got exactly one info line per hub.
- Episode 2 alone doesn't prove the fix, because hub-b won it, so hub-a's
  refusal there was its first ever. Episode 3 is the proof: with two hubs, its
  loser had to repeat an earlier refusal exactly.
- Coordination itself was unchanged: 1 sync per commit (`syncs.txt`), and the
  pin advanced by the winning hub each time.

## Gaps

- The live run covered `held` only. Expiry for errors, pin errors and the
  waiting-operation Event is covered by unit tests, not observed live.
- The dedup map still grows by one entry per (app, path) that has ever been
  refused, never pruned. That's fine at lab scale but would need pruning on a
  hub with thousands of apps.
