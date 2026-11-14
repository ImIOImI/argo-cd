# Multi-hub research

Findings from the research spike into running Argo CD application controllers
active/active on two hubs against the same Applications and spoke clusters.

- **Design:** [docs/proposals/multi-hub-active-active.md](../docs/proposals/multi-hub-active-active.md)
- **Lab:** [hack/multi-hub-lab](../hack/multi-hub-lab) (kind: hub-a, hub-b, spoke)

## Log

| Date | Note | Summary |
|------|------|---------|
| 2026-11-14 | [Baseline: stock Argo CD on two hubs](2026-11-14-baseline-stock-argo.md) | Stock v3.6.1 churns for ~2 min on every commit with self-heal on (11 syncs for 1 commit), and starts duplicate full syncs, with hooks, when both hubs see OutOfSync together. With no change to rendered manifests, nothing gets written. A manual rollback on one hub is reverted by the other within 1s while the first hub reports success. |
| 2026-11-14 | [C2: operation lease on the destination cluster](2026-11-14-c2-operation-lease.md) | A per-app Lease on the spoke serializes operations: when both hubs discover a commit together, 1 hub syncs (2 with the lease off). Skew flapping drops only from 11 to 9 syncs, now taking turns instead of overlapping, so the revision pin (C1) is still required. |
| 2026-11-14 | [C2 telemetry: logs, Events, metrics + settings](2026-11-14-c2-telemetry.md) | Every lease decision is a structured log line, a metric sample and, for takeover, lost lease, errors and waiting operations, a Kubernetes Event. Repeated refusals collapse to one line per change. Five env settings quiet or expand routine output, but can't silence warnings. Verified live with defaults and with settings switched; also the first live test of a manual sync waiting behind another hub's lease. |

Raw data for each note is in `data/<date>-<slug>/`.

**Process:** from `fb2b65383` on, all implementation is test-driven, with a
red test commit before each green one, proven by `hack/tdd/red-green.sh`
(rules in `CLAUDE.md`). The C2 lease and C2 telemetry notes describe code
written **test-after**, before that rule existed.

## Running scoreboard

What the fork has to fix. Each column is re-measured as each coordination
change (C1–C6 in the design doc) lands. The fork columns run the fork's binary
(master) on v3.6.1 images.

| Problem | Stock v3.6.1 | Fork, C2 | Target |
|---------|--------------|----------|--------|
| Writes to the spoke when nothing changed (E1) | 0 | — | 0 |
| Syncs per commit, self-heal on (E2) | 11 (6 hub-a, 5 hub-b), overlapping | 9 (5 / 4), serialized | 1 |
| Time the hubs disagree after a commit (E2) | ~4m50s | flapping ~1.5 min | ≈ 0 |
| Spoke shows the *old* generation after a commit | possible: last write raced at 12:02:32 | still possible (hub-b reverts on its turn) | never |
| Full syncs when both hubs discover a commit together (E7) | 2 (same binary, lease off) | **1** | 1 |
| Hook runs in that case | 2 if starts are ≥1s apart (R0), 1 if same second (E7 control) | **1** | 1 |
| Syncs per commit, self-heal off (E4) | 1 (hub-b skips), but drift is never corrected | — | 1, with self-heal on |
| Manual rollback on one hub survives (E5) | no: reverted by the other hub in 1s, and the rolling-back hub still shows Succeeded + Synced | — | yes, on every hub, until cleared |

"—" = not re-measured for that change.
