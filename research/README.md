# Multi-hub research

Findings from the research spike into running Argo CD application controllers
active/active on two hubs against the same Applications and spoke clusters.

- **Design:** [docs/proposals/multi-hub-active-active.md](../docs/proposals/multi-hub-active-active.md)
- **Lab:** [hack/multi-hub-lab](../hack/multi-hub-lab) (kind: hub-a, hub-b, spoke)

## Log

| Date | Note | Summary |
|------|------|---------|
| 2026-11-14 | [Baseline: stock Argo CD on two hubs](2026-11-14-baseline-stock-argo.md) | Stock v3.6.1 churns for ~2 min on every commit with self-heal on (11 syncs for 1 commit), and starts duplicate full syncs, with hooks, when both hubs see OutOfSync together. With no change to rendered manifests, nothing gets written. |

Raw data for each note is in `data/<date>-<slug>/`.

## Running scoreboard

What the fork has to fix, measured on stock Argo CD. Each row gets re-run as
each coordination change (C1–C6 in the design doc) lands.

| Problem | Stock v3.6.1 | Target |
|---------|--------------|--------|
| Writes to the spoke when nothing changed (E1) | 0 | 0 |
| Syncs per commit, self-heal on (E2) | 11 (6 hub-a, 5 hub-b) | 1 |
| Time the hubs disagree after a commit (E2) | ~4m50s | ≈ 0 (bounded by the lease) |
| Spoke shows the *old* generation after a commit | possible: last write raced at 12:02:32 | never |
| Hook runs per full sync started by both hubs (initial sync) | 2 | 1 |
| Syncs per commit, self-heal off (E4) | 1 (hub-b skips), but drift is never corrected | 1, with self-heal on |
| Manual rollback survives on the other hub (E5) | *not run yet* | yes |
