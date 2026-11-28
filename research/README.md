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
| 2026-11-14 | [C1: revision pin shared through the destination](2026-11-14-c1-revision-pin.md) | First TDD feature (4 red → green pairs, all proven). Both hubs compare and sync against one agreed revision, stored on the app's Lease and ordered by commit time. One commit with self-heal on now causes **1 sync** (stock 11, C2 alone 9). The stale hub adopts the pin within ~1s and never reverts. Found that refusal-log dedup never expires. |
| 2026-11-20 | [Refusal-log dedup expires after a quiet gap](2026-11-20-refusal-dedup-expiry.md) | Fixes the C1 finding: a refusal that repeats an earlier one is logged at info again after `ARGOCD_MULTIHUB_LOG_DENIALS_RESET_AFTER` (default 2m) of quiet, while a continuous episode stays collapsed. 2 red → green pairs. Verified live over three E7 episodes. |
| 2026-11-20 | [C3: taking over an operation when its hub dies](2026-11-20-c3-takeover.md) | The holder mirrors its operation state on the Lease. When it dies, the other hub resumes **the same operation**, so a PreSync hook killed mid-run runs **once** (control: twice, overlapping). A hub that comes back takes the result instead of re-running (control: it re-ran the sync phase). The first live run found a bug the unit tests missed (a stale `finishedAt` turned the takeover into a retry); it was fixed test-first and re-run. 5 red → green pairs. |
| 2026-11-20 | [C3 gaps: faster takeover, manual syncs, mid-rollout death](2026-11-20-c3-gaps.md) | A hub schedules a refresh at the other hub's lease expiry, so a takeover lands 3–5s after expiry even when nothing else would refresh the app (the first live run found it didn't, and was fixed test-first). A manual sync is taken over with its initiator kept. A hub killed mid-rollout is resumed on the same rollout, with wave order kept and the hook not re-run. A failed claim is given back; the returning hub keeps the real `finishedAt`; unchanged mirrors aren't rewritten. 5 red → green pairs. |
| 2026-11-20 | [C5: a rollback on one hub holds on every hub](2026-11-20-c5-override.md) | E5 re-run: a rollback on hub-b records an override on the Lease, and every hub's auto-sync (self-heal included) stands down until a user sync to HEAD on any hub. The rollback held through a 4-minute watch (stock: reverted in 1s), and both hubs honestly show OutOfSync (stock: the rolling-back hub said Synced). 4 red → green pairs. |
| 2026-11-21 | [C7: two hubs share the refresh work](2026-11-21-c7-shared-refresh.md) | A hub whose periodic refresh comes due uses another hub's fresh, published result (from a local watch of the Leases) instead of comparing again. With 11 apps, each hub did 54–58% of its unshared work, while each app was still refreshed at least as often as by one hub alone. With one hub stopped, the other went back to full rate with no step. Each commit was still synced exactly once per app, by one hub. 5 red → green pairs. |
| 2026-11-21 | [One line in Git sets both hubs' refresh period, with no restart](2026-11-21-live-refresh-period.md) | With multihub on, the controller reads `timeout.reconciliation` from `argocd-cm` live. A root app manages just that key with server-side apply, so each hub keeps its own settings. Changing it from 60s to 120s in Git reached both hubs in 96s, and both slowed accordingly (each app refreshed every ~92s → ~141s), with no controller restart. 2 red → green pairs. |
| 2026-11-22 | [Three hubs: coordination holds, but refresh sharing doesn't balance](2026-11-22-three-hubs.md) | With hub-c added (no new fork code): a commit discovered by all three syncs once per app with one hook; a hub killed mid-hook holding 11 syncs has each claimed by exactly one of two racing survivors, with every hook run once. Shared refresh does one hub's worth of work in total, but one hub did ~76% of it, because the skip rule keeps the lead with whoever has it. Needs a balancing rule built on the hub list. |
| 2026-11-22 | [Hub membership: declared in Git, live on every hub](2026-11-22-hub-membership.md) | A `HubMembership` CRD declares members by id or label selector, each `active` or `watching`, plus the refresh period and `forgetAfter`, validated at apply time. Each hub resolves its own state from its self-registration labels and publishes a record on every spoke. Live: a watching hub starts no syncs, comes back when set active, and a hub retired mid-sync finishes that sync and then stops. Found and fixed live: the gate also froze a watching hub's own root app. Open: one commit was applied by partial syncs that skipped hooks (R4). 10 red → green pairs. |
| 2026-11-28 | [R4: a commit compared mid-sync is later synced partially, skipping hooks (upstream bug)](2026-11-28-r4-skipped-hooks.md) | Reproduced on one hub with coordination off and on stock v3.6.1: upstream argoproj/argo-cd#27875 (fix proposed in #28908, both open). Auto-sync's change detection compares against the last *compared* revision, so a commit compared but not synced is later treated as already attempted and self-healed with a partial sync (no hooks). The fork makes "compared but not synced" common (lease held, settle window, override, watching, shared refresh). Fixed in the fork test-first (option 2): before "already attempted", auto-sync re-checks against the last *synced* revision (the spoke's, across hubs). Verified: a full sync with hooks for a commit compared mid-sync, on one hub and on three; drift is still a hook-less self-heal. |

Raw data for each note is in `data/<date>-<slug>/`.

**Process:** from `fb2b65383` on, all implementation is test-driven, with a
red test commit before each green one, proven by `hack/tdd/red-green.sh`
(rules in `CLAUDE.md`). The C2 lease and C2 telemetry notes describe code
written **test-after**, before that rule existed.

## Running scoreboard

What the fork has to fix. Each column is re-measured as each coordination
change (C1–C6 in the design doc) lands. The fork columns run the fork's binary
(master) on v3.6.1 images.

| Problem | Stock v3.6.1 | Fork, C2 | Fork, C1 + C2 | Fork, C1 + C2 + C3 (+ C5) | Target |
|---------|--------------|----------|---------------|--------------------|--------|
| Writes to the spoke when nothing changed (E1) | 0 | — | — | — | 0 |
| Syncs per commit, self-heal on (E2) | 11 (6 hub-a, 5 hub-b), overlapping | 9 (5 / 4), serialized | **1** (1 / 0) | — | 1 |
| Time the hubs disagree after a commit (E2) | ~4m50s | flapping ~1.5 min | **~1s** (pin set → adopted) | — | ≈ 0 |
| Spoke shows the *old* generation after a commit | possible: last write raced at 12:02:32 | still possible (hub-b reverts on its turn) | **no revert observed** | — | never |
| Full syncs when both hubs discover a commit together (E7) | 2 (same binary, lease off) | **1** | **1** | — | 1 |
| Hook runs in that case | 2 if starts are ≥1s apart (R0), 1 if same second (E7 control) | **1** | **1** | — | 1 |
| Syncs per commit, self-heal off (E4) | 1 (hub-b skips), but drift is never corrected | — | — | — | 1, with self-heal on |
| Manual rollback on one hub survives (E5) | no: reverted by the other hub in 1s, and the rolling-back hub still shows Succeeded + Synced | — | — | **yes** (with C5): held until a sync to HEAD; both hubs show OutOfSync, and the other hub raises `MultihubAutoSyncPaused` | yes, on every hub, until cleared |
| Hook runs when the syncing hub dies mid-hook (E8) | — | — | 2, overlapping (control) | **1** (adopted) | 1 |
| Lease expiry → takeover, app Synced on the surviving hub (E9) | — | — | — | **5s** (34s before the E9 fix) | ≈ 0 |
| Change the refresh period on both hubs | edit `argocd-cm` on each hub + restart each controller | — | — | **one line in Git**, both hubs in ~96s, no restart | one change, no disruption |
| Full refreshes per hub, two hubs (E11) | 100% each (both do everything) | — | — | **54–58% each** with C7; 100% on the survivor if one dies | 50% |
| Death mid-rollout, sync phase (E10) | — | — | — | **same rollout resumed**, wave order kept, hook not re-run | resume |
| Hub that died comes back after the other finished (E8) | — | — | resumes its stale operation, re-runs the sync phase | **takes the result**, runs nothing | no repeat |

"—" = not re-measured for that change.
