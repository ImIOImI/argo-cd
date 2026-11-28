# R4: a commit compared mid-sync is later synced partially, skipping hooks (upstream bug)

**Date:** 2026-11-28 · **Images:** fork `argocd-multihub:1c4d21255` with multihub **off**, and stock
`quay.io/argoproj/argocd:v3.6.1` · **No fork code changed**

## Summary

The open finding R4 from
[2026-11-22-hub-membership.md](2026-11-22-hub-membership.md) was that one
commit reached 9 of 11 apps through partial syncs, so their PreSync hooks
didn't run.

- **It reproduces on a single hub, with coordination off and on stock Argo
  CD v3.6.1.** It's an **upstream bug**, tracked as
  [argoproj/argo-cd#27875](https://github.com/argoproj/argo-cd/issues/27875).
  A fix is proposed in
  [#28908](https://github.com/argoproj/argo-cd/pull/28908). Both were open as
  of 2026-11-28.
- **The fork doesn't cause it, but makes the trigger much more common.**

## Mechanism (from the code)

1. **Argo detects new changes against the wrong revision.**
   - Auto-sync decides "new revision with changes, so do a full sync" using
     `revisionsMayHaveChanges`.
   - For apps with `argocd.argoproj.io/manifest-generate-paths`,
     `evaluateRevisionChanges` (`controller/state.go`) computes that by
     asking the repo-server whether files under the app's paths changed
     between `app.Status.Sync.Revision` and the new revision.
   - `Status.Sync.Revision` is the last revision **compared**, not the last
     one **synced**.
2. **A refresh can compare a new commit without syncing it.** That sets
   `Status.Sync.Revision` to the new commit. Upstream, that happens when an
   operation is still running.
3. **The next refresh compares the new commit with itself.** It finds no
   changes, and `alreadyAttemptedSync` then skips the revision comparison
   and compares only the source, which is unchanged. The result is "already
   attempted".
4. **With self-heal on, auto-sync then issues a partial sync** of the
   OutOfSync resources. Hooks don't run for partial syncs. With self-heal
   off, the commit isn't synced at all (the symptom reported in #27875).

## Why the fork makes it more likely

Upstream, "compared but not synced" needs an operation to be running when
the new commit is compared. In the fork, it also happens whenever a hub
compares a new commit but doesn't sync it:
- another hub holds the operation lease (C2);
- the settle window is open;
- a rollback override is in place (C5);
- the hub is `watching` or not a member;
- a shared refresh was copied from another hub (C7), which brings that
  hub's `Sync.Revision` with it.

With three hubs, most refreshes of a new commit happen on hubs that don't
sync it, which fits 9 of 11 apps in R4.

## How it was run

Lab scripts are in `hack/multi-hub-lab/` and were run from that directory.
Only hub-a was running (hub-b's and hub-c's controllers were stopped).

| Step | Command | Script (pinned) |
|---|---|---|
| Fork binary, multihub off | `./controller.sh hub-b stop; ./controller.sh hub-c stop; MULTIHUB=off ./deploy-fork.sh` | [`deploy-fork.sh` @ 188f7a822](https://github.com/ImIOImI/argo-cd/blob/188f7a822/hack/multi-hub-lab/deploy-fork.sh), [`controller.sh` @ fcc7bafb4](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/controller.sh) |
| **Reproduction** | `HUB=hub-a ./repro-r4.sh` | [`repro-r4.sh` @ 0be465101](https://github.com/ImIOImI/argo-cd/blob/0be465101/hack/multi-hub-lab/repro-r4.sh) |
| Stock v3.6.1 on hub-a (every Argo container's image swapped back; one-off `kubectl get … -o json` / image rewrite / `apply`, the same rewrite `deploy-fork.sh` does) | then `./refresh.sh hub-a && ./wait-refreshed.sh` | [`refresh.sh` @ fcc7bafb4](https://github.com/ImIOImI/argo-cd/blob/fcc7bafb4/hack/multi-hub-lab/refresh.sh) |
| **Reproduction on stock** | `HUB=hub-a ./repro-r4.sh` | as above |
| Lab restored (fork, multihub on, three hubs, hub-c re-registered `role=hub`, hook back to 5s) | `./deploy-fork.sh; ./controller.sh hub-b start; ./controller.sh hub-c start; HUBS=hub-c ./self-register.sh; ./hook-sleep.sh 5 && git push` | |

Upstream status was checked with `gh pr view 28908 -R argoproj/argo-cd` and
`gh issue view 27875 -R argoproj/argo-cd` on 2026-11-28.

Raw data: [data/2026-11-28-r4-skipped-hooks](data/2026-11-28-r4-skipped-hooks).

## Results

**Fork, multihub off:**
```
mid-sync: compared=51479a5… status=OutOfSync op=Running op-rev=4682c1d…   <- new commit compared, not synced
== lab-app operations
  02:26:34 rev 4682c1d full
  02:28:51 rev 51479a5 PARTIAL (no hooks)
== hook Jobs: lab-presync-4682c1d-… only
```

**Stock Argo CD v3.6.1:**
```
mid-sync: compared=1305128… status=OutOfSync op=Running op-rev=6135a0d…
== lab-app operations
  02:33:16 rev 6135a0d full
  02:35:31 rev 1305128 PARTIAL (no hooks)
== hook Jobs: lab-presync-6135a0d-… only
```

Same sequence, same outcome: the second commit was applied without its
PreSync hook. It also doesn't show in History or Rollback as a sync of that
commit's hooks.

## Options

1. **Carry upstream's fix (#28908) in the fork** until it's merged.
   - Its approach: base change detection on the last *synced* revision
     (`OperationState.SyncResult`). It's large: 8 files and about 2,000
     changed lines, including a repo-server protocol change.
   - **Pro:** when upstream merges it, the fork drops it on the next rebase.
   - **Con:** it's still under review and may change; carrying it now means
     re-syncing with each revision.
2. **A small fork-only fix in `autoSync`.**
   - If the "no changes" verdict was computed against a revision other than
     the last one synced, re-check whether the app's paths changed between
     the **last synced** revision and the desired one. That uses the
     existing `UpdateRevisionForPaths` RPC with `SyncedRevision` = the last
     synced revision, so there's no protocol change.
   - The extra RPC happens only in that case.
   - **Pro:** about 30 lines plus tests.
   - **Con:** fork-specific until upstream's fix lands; then it's removed.
3. **No code: drop `manifest-generate-paths` from the apps.**
   - Change detection then always says "may have changes", so a new commit
     is always a full sync.
   - **Cost:** the monorepo optimization is lost, so every commit to the
     repo looks like a change to every app. Combined with self-heal, that
     can mean extra syncs.

## Status

Not fixed in the fork yet. The decision is with Troy.
