# 2026-11-14: Baseline, stock Argo CD on two hubs

**Question:** what actually happens today when two unmodified Argo CD hubs
both run auto-sync on the same Application?

**Setup** ([hack/multi-hub-lab](../hack/multi-hub-lab), commit `e78ab670f`)
- kind v0.30.0, three clusters: `hub-a`, `hub-b`, `spoke`.
- Argo CD **v3.6.1** stock install on both hubs, same `installationID`
  (`multi-hub-lab`), annotation tracking.
- `lab-app` defined the same way on both hubs: tracks branch
  `spike/multi-hub`, path `hack/multi-hub-lab/app`, automated
  `prune: true, selfHeal: true`, `manifest-generate-paths: .`.
- App contents: ConfigMap `lab-config` (with a `generation` counter), a pause
  Deployment, and a PreSync hook Job using `generateName`.
- Skew by design: hub-a reconciles every **30s**, hub-b every **180s**. No
  Git webhooks.
- Watched with `observe.sh` (a sample about every 4s) plus each controller's
  log of auto-sync decisions. The controller logs are the reliable source:
  the samples are too coarse to catch quick flips.

Raw data: [data/2026-11-14-baseline](data/2026-11-14-baseline)

## How it was run

All scripts are in `hack/multi-hub-lab/` and were run from that directory,
on WSL2 with docker 29.4.3, kind v0.30.0 and kubectl v1.34.1. Links are pinned
to the commit each script was at when it was used.

| Step | Command | Script (pinned) |
|---|---|---|
| Bring up the lab | `./up.sh` | [`up.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/up.sh) |
| Fix for the first fetch timing out (run by hand on both hubs, then added to `up.sh`) | timeout patch + controller restart | [`up.sh` @ e78ab670f](https://github.com/ImIOImI/argo-cd/blob/e78ab670f/hack/multi-hub-lab/up.sh), lines 45–50 |
| Wait for both hubs to sync | `./wait-synced.sh` | [`wait-synced.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/wait-synced.sh) |
| Sample spoke + hubs (background, whole session) | `rm -f observe.log; INTERVAL=2 ./observe.sh` | [`observe.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/observe.sh) |
| E1: steady state | no action, 4 min | — |
| E2: one commit, self-heal on | `./bump.sh` (pushed `12c0494`) | [`bump.sh` @ e26421477](https://github.com/ImIOImI/argo-cd/blob/e26421477/hack/multi-hub-lab/bump.sh) |
| Sync timeline (R2 table) | `AFTER=2026-11-14T12:00 ./syncs.sh` | [`syncs.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/syncs.sh) |
| E4: self-heal off, one commit | `./selfheal.sh off && ./bump.sh` (pushed `f3e8959`) | [`selfheal.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/selfheal.sh) |
| Save raw data (E1–E4) | `./collect.sh ../../research/data/2026-11-14-baseline` | [`collect.sh` @ c09328a9f](https://github.com/ImIOImI/argo-cd/blob/c09328a9f/hack/multi-hub-lab/collect.sh) |
| E5: sample (background) | `rm -f observe.log && exec ./observe.sh` | [`observe.sh` @ ded013cef](https://github.com/ImIOImI/argo-cd/blob/ded013cef/hack/multi-hub-lab/observe.sh) |
| E5: self-heal on for hub-a only, then roll back hub-b to history id 0 | `HUBS=hub-a ./selfheal.sh on && ./rollback.sh hub-b 0` | [`selfheal.sh` @ ded013cef](https://github.com/ImIOImI/argo-cd/blob/ded013cef/hack/multi-hub-lab/selfheal.sh), [`rollback.sh` @ ded013cef](https://github.com/ImIOImI/argo-cd/blob/ded013cef/hack/multi-hub-lab/rollback.sh) |
| E5: sync timeline | `AFTER=2026-11-14T12:35 ./syncs.sh` | [`syncs.sh` @ ded013cef](https://github.com/ImIOImI/argo-cd/blob/ded013cef/hack/multi-hub-lab/syncs.sh) |
| E5: save raw data | `SINCE=15m ./collect.sh ../../research/data/2026-11-14-baseline/e5` | [`collect.sh` @ ded013cef](https://github.com/ImIOImI/argo-cd/blob/ded013cef/hack/multi-hub-lab/collect.sh) |

**Provenance:** `wait-synced.sh`, `syncs.sh`, `selfheal.sh` and
`collect.sh` were first run as one-off commands during the session. They
were saved as scripts afterwards (c09328a9f) with the same commands, and
`syncs.sh` was re-run to check that it reproduces the R2 table exactly.
Everything else was run from the pinned commit as written. The E5 scripts
were committed (ded013cef) *before* they were run.

---

## R0. Initial sync: both hubs ran the PreSync hook

Each hub started its own full sync of the new app, 19s apart (hub-b at
11:55:00Z, hub-a at 11:55:19Z). Two hook Jobs ran:

```
lab-presync-e264214-presync-1794657300
lab-presync-e264214-presync-1794657319
```

The names differ only in the `startedAt` suffix. This confirms design-doc
findings **F3** (neither hub sees the other's operation) and **F5** (hook
names are `<generateName><sha7>-<phase>-<startedAt>`). A database migration
in a PreSync hook would have run twice.

## R1 (E1). Steady state: no writes, even across a commit that changes nothing

4.3 minutes, 63 samples, ConfigMap `resourceVersion` stayed at `1127`.

During that window commit `e78ab670f` landed. It changed only the lab
*scripts*, not the app path. hub-a moved to `e78ab67` while hub-b stayed on
`e264214`, so **the hubs were on different revisions and still didn't
fight**, because both rendered the same manifests.

**Conclusion:** Argo decides by comparing content. Running two hubs costs
nothing while their desired state matches. Churn needs a difference in
*rendered* output, not just a different SHA.

## R2 (E2). One real commit, self-heal on: 11 syncs over 2 minutes, ~5 minutes of disagreement

Pushed generation 1→2 (`12c0494`) at 12:00:35Z (UTC; local 06:00:35).

Every sync the controllers started (from `Initialized new operation`):

| Time (UTC) | hub-a → | hub-b → | Self-heal attempt |
|---|---|---|---|
| 12:00:46 | `12c0494` (gen 2), full sync | | 0 |
| 12:01:03 | | `e78ab67` (gen 1) | 1 |
| 12:01:04 | `12c0494` | | 1 |
| 12:01:06 | `12c0494` | `e78ab67` | 2 |
| 12:01:12–13 | `12c0494` | `e78ab67` | 3 |
| 12:01:33–34 | `12c0494` | `e78ab67` | 4 |
| 12:02:32 | `12c0494` | `e78ab67` | 5 |

- Same mechanism as **F2**: hub-a's apply changed the spoke, which made
  hub-b do a `CompareWithRecent` against its *own* last-resolved commit
  (`e78ab67`). Hub-b saw OutOfSync and self-healed back to generation 1.
  Hub-a did the same in reverse.
- **Self-heal backoff was the only damping** (gaps of about 2s, 6s, 21s and
  58s). The next rounds would have been further apart, and hub-b resolved the
  new commit first: both hubs showed `Synced 12c0494` at about 12:05:28.
- **The final state came down to timing.** Both hubs wrote in the same second
  at 12:02:32. Generation 2 happened to land last. If hub-b's write had landed
  last, the spoke would have run the **old config for about 3 minutes** while
  hub-a showed it as Synced. hub-a's next self-heal would have fixed it, but
  only after its own backoff.
- ConfigMap `resourceVersion` went 1127 → 1599 → 1604 → 1614 → 1640 → 1714.
  The ~4s samples always showed generation 2. Every flip to generation 1 was
  reverted between samples, so **polling the spoke's state hides this churn**.
  Only the controller logs or audit logs show it.
- **No extra hook runs:** self-heal syncs only re-apply the out-of-sync
  resources (`op.Sync.Resources` is set in `autoSync`), so PreSync didn't run.
  One hook Job for `12c0494`. Duplicate hooks come from *full* syncs that both
  hubs start (R0), not from flapping.

## R3 (E4). Self-heal off

`./selfheal.sh off && ./bump.sh` at 12:07:48Z (generation 2→3, `f3e8959`),
then watched for 6 minutes. **Hypothesis H1 confirmed.**

`AFTER=2026-11-14T12:07 ./syncs.sh`:
```
== hub-a
2026-11-14T12:08:11Z rev=f3e8959 selfHealAttempt=0
== hub-b
```

- **1 sync for 1 commit**, plus 1 hook run (`lab-presync-f3e8959-presync-1794658091`).
  ConfigMap ended at generation 3.
- hub-b did see the spoke as OutOfSync against its old commit, but it
  declined to act:
  `12:08:25Z Skipping auto-sync: most recent sync already to [12c0494…]`
  (the `alreadyAttemptedSync` path in `autoSync`).
- hub-b then caught up and both hubs reported `Synced` on the branch's
  current HEAD, without hub-b ever syncing.

**But this isn't a fix.** With self-heal off, real drift (someone running
`kubectl edit` on the spoke) is never corrected by either hub, which defeats
goal 2. What R3 shows is that self-heal is exactly what turns revision skew
into flapping. The revision pin (C1) has to make self-heal safe, not
remove it.

## R4 (E5). Manual rollback on one hub: undone in 1 second, and hub-b still reports success

Starting state: both hubs `Synced`, spoke on generation 3. Then:
`HUBS=hub-a ./selfheal.sh on && ./rollback.sh hub-b 0`. Hub-b turned
off its own auto-sync (rollback requires it) and rolled back to history id 0
(`e264214`, generation 1). Hub-a kept auto-sync with self-heal on.
`rollback.sh` writes the same Operation as `(*Server).Rollback`
(`server/application/application.go:2341`): the history entry's
revision and source, apply strategy, `prune: false`.

`AFTER=2026-11-14T12:35 ./syncs.sh`:
```
== hub-a
2026-11-14T12:35:58Z rev=8a8132a selfHealAttempt=1
== hub-b
2026-11-14T12:35:57Z rev=e264214 selfHealAttempt=0
```

- **The rollback lasted about 1 second.** Hub-b applied generation 1 at
  12:35:57Z, and hub-a self-healed it back to the branch HEAD at 12:35:58Z.
  The spoke ended on generation 3 (ConfigMap `resourceVersion` 2175 → 4183).
- **Hub-b reported success anyway.** Afterwards hub-b showed
  `Synced ded013c, op=Succeeded, initiatedBy=lab-rollback`. Its UI says the
  rollback succeeded and the app is in sync, while the spoke is running the
  version the operator tried to roll back *from*. Hub-b with auto-sync off
  compares against the branch HEAD, which matches the live state again, so
  nothing on hub-b points to a problem. It's a silent failure: the worst
  outcome for an operator responding to an incident.
- The ~4s samples never caught generation 1 on the spoke. Only the
  controller logs show it happened.
- No hook ran for the rollback, as expected: rollback uses the apply sync
  strategy, which skips hooks.

This confirms **F6**: manual operations stay on the hub where they were
made. In a real incident, "roll back from whichever hub I'm logged into"
(goal 1) silently does nothing while the other hub has self-heal on.

---

## What this means for the design

1. **The revision pin (C1) is the main fix.** R1 shows the hubs get along
   whenever they agree on what to render. R2 shows that all the churn comes
   from the skew window.
2. **The operation lease (C2) fixes R0** (duplicate full syncs and hooks),
   and on its own it would also have removed most of R2: hub-b could not have
   started a revert while hub-a held the lease. But once the lease was
   released, hub-b would still revert. So C2 does not replace C1.
3. **Coordinated manual operations (C5) are a correctness requirement, not
   a convenience.** R4 shows a rollback on one hub being reverted within a
   second while that hub reports success. The pin with `override: true`
   has to be honored by *every* hub before a rollback can be trusted.
4. **Checking for churn has to use controller or audit events, not spoke
   polling.** The lab should count `Initialized new operation` per commit
   (now the scoreboard metric in [README](README.md)).
5. Lab note: tracking a branch of the large argo-cd fork needs a longer
   repo-server timeout (`ARGOCD_EXEC_TIMEOUT`,
   `controller.repo.server.timeout.seconds`) for the first fetch. Now in
   `up.sh`.
