# controller/multihub

Coordinates Argo CD installations ("hubs") that manage the same Applications
on the same destination clusters. Design:
[docs/proposals/multi-hub-active-active.md](../../docs/proposals/multi-hub-active-active.md).
Measurements: [research/](../../research/).

Implemented so far:
- **C2, the operation lease:** one `coordination.k8s.io/v1` Lease per
  Application, on the destination cluster, so at most one hub runs a sync
  operation for an Application at a time.
- **C1, the revision pin:** annotations on that same Lease record the
  revision every hub compares and syncs against. After each comparison a hub
  offers its resolved revision. The pin only moves to a strictly later
  commit (by commit time, read from the repo-server). A hub that's behind
  compares again at the pin instead of reverting. Not applied to
  multi-source, helm or OCI apps.
- **C3, takeover:** the lease holder mirrors its operation state onto the
  same Lease after every pass. If it dies, another hub claims the running
  operation once the lease expires and resumes **the same operation** (same
  `startedAt`, initiator and per-resource progress), so a running hook is
  adopted, not run again. A hub that comes back with a stale copy of that
  operation follows the other hub's state instead of resuming it. A hub that
  sees another hub's live lease schedules a refresh for when it expires, so
  the takeover doesn't wait for a periodic refresh.
- **C5, coordinated rollback:** a user's full sync to a revision other than
  the app's `targetRevision` (Argo's Rollback is one) records an
  **override** on the Lease before syncing. While it exists, every hub's
  automated sync (self-heal included) stands down. A user sync to the
  `targetRevision` on any hub clears it.
- **C7, shared refresh:** after a full refresh a hub publishes its result
  (record on the Lease, result in `<lease>-result`). A hub whose periodic
  refresh comes due shows another hub's fresh result instead of comparing,
  if both have the same app definition (fingerprint) and the result is at
  the pinned revision. The check reads a local watch of the Leases.

- **Hub membership:** a `HubMembership` resource (CRD in
  `manifests/crds/`, one object named `default` in the Argo namespace, synced
  from Git) declares which hubs are members, by id or by a label selector on
  each hub's own cluster self-registration, and whether each is `active` or
  `watching`. It also holds `refreshPeriod` and `forgetAfter`. A watching or
  non-member hub starts no work for shared apps but still manages its own
  cluster. Each hub publishes a record of itself (`hub.<id>` Lease) on every
  spoke. Ids must be distinct per hub; the fork doesn't check. Example:
  `hack/multi-hub-lab/bootstrap/hubmembership.yaml`.

## Configuration

All settings are environment variables on the **application controller**
(`argocd-application-controller` StatefulSet). Coordination is off, and the
controller behaves exactly like upstream, unless `ARGOCD_MULTIHUB_HUB_ID` is
set.

| Variable | Default | Meaning |
|---|---|---|
| `ARGOCD_MULTIHUB_HUB_ID` | *(unset = off)* | Name of this hub. Must be unique across hubs sharing destinations. |
| `ARGOCD_MULTIHUB_NAMESPACE` | `argocd-coordination` | Namespace on each destination cluster that holds the Leases (created on demand). |
| `ARGOCD_MULTIHUB_LEASE_DURATION` | `60s` | A lease not renewed for this long can be taken over by another hub. Keep it well above 30s (the longest gap between an operation's passes). |
| `ARGOCD_MULTIHUB_SETTLE_WINDOW` | `10s` | After another hub releases a lease, *auto*-sync on this hub waits this long so it re-compares against the finished state. User-started operations don't wait. |
| *(argocd-cm)* `timeout.reconciliation` | startup value | With multihub on, the periodic refresh period is read **live** from argocd-cm (no restart). Manage that one key from Git with server-side apply to set every hub at once (see `hack/multi-hub-lab/bootstrap/`). |
| `ARGOCD_MULTIHUB_SHARE_REFRESH` | `true` | Skip a periodic refresh that another hub just did and show its result (C7). |

### Telemetry

| Variable | Default | Effect |
|---|---|---|
| `ARGOCD_MULTIHUB_LOG_LIFECYCLE` | `true` | Log acquiring and releasing the lease at info. `false` = debug. |
| `ARGOCD_MULTIHUB_LOG_RENEWALS` | `false` | Log each renewal (every pass of a running operation) at info. `false` = debug. |
| `ARGOCD_MULTIHUB_LOG_DENIALS` | `changes` | Refusals ("held by hub-b", "settling"): `changes` = info when the event or holder changes for an app, debug for repeats; `all` = info every time; `off` = debug. |
| `ARGOCD_MULTIHUB_LOG_DENIALS_RESET_AFTER` | `2m` | With `changes`: a repeat refusal or error is reported again once none has been seen for this long, so each new episode gets one info/warn line (and one waiting-operation Event). A continuous episode stays collapsed however long it lasts. `0` = never reset. |
| `ARGOCD_MULTIHUB_EVENTS` | `true` | Kubernetes Events on the Application (table below). |
| `ARGOCD_MULTIHUB_METRICS` | `true` | Export `argocd_multihub_lease_decisions_total`. |

**Always on, whatever the settings:** warnings for a **takeover**, a **lost
lease** and **lease errors**. Errors are collapsed like refusals (a repeat of
the same error goes to debug) but are never dropped below warn when they
change. Anything a setting switches "off" is logged at **debug**, not
dropped: `--loglevel debug` always shows everything.

Invalid values: booleans fall back to their default. An unknown
`LOG_DENIALS` falls back to `changes` with a startup warning.

## What gets emitted

### Logs

Every lease decision is one log line from the application controller. It
has the standard app fields (`application`, `app-namespace`, `project`)
plus:

| Field | Value |
|---|---|
| `multihub.hub` | this hub |
| `multihub.path` | `auto-sync`, `operation`, `takeover` (claiming another hub's orphaned operation) `override` (C5) or `refresh` (C7) |
| `multihub.event` | `acquired`, `renewed`, `takeover`, `held`, `settling`, `released`, `lost`, `error`, `release_error`, `mirror_error`, `following`, `override_set`, `override_cleared`, `overridden`, `shared` |
| `multihub.holder` | hub holding the lease (or that just released it, for `settling`) |
| `multihub.generation` | fencing token (`leaseTransitions`), when granted |
| `multihub.reason` | free-text reason / error |
| `multihub.retry_after` | for refusals: when retrying may succeed |

Messages keep upstream wording where one exists, e.g.
`Skipping auto-sync: operation lease held by hub-a (rechecking in 15s)`.

| Event | Level |
|---|---|
| `acquired`, `released` | info (`LOG_LIFECYCLE`) |
| `renewed` | debug (info with `LOG_RENEWALS`) |
| `held`, `settling`, `following`, `overridden` | per `LOG_DENIALS` |
| `override_set`, `override_cleared` | info |
| `shared` | debug (one per skipped refresh; the `Reconciliation completed` line also carries `multihub.shared=true`) |
| `takeover`, `lost` | **warn** |
| `error`, `release_error`, `mirror_error` | **warn** on change, debug for repeats (warn every time with `LOG_DENIALS=all`) |

Startup logs one line with the effective configuration:
`multihub coordination enabled: hub id "hub-a"` with `multihub.*` fields
for every setting.

Revision-pin decisions use the same fields with `multihub.path=pin`, plus
`multihub.candidate_revision` (what this hub resolved),
`multihub.pinned_revision` and `multihub.pinned_by`:

| Pin event | Level |
|---|---|
| `pinned`, `advanced`, `adopted` | info (`LOG_LIFECYCLE`) |
| `kept` | debug |
| `error` | **warn** on change, debug for repeats (warn every time with `LOG_DENIALS=all`) |

### Kubernetes Events (on the Application)

| Reason | Type | When |
|---|---|---|
| `MultihubLeaseTakeover` | Warning | this hub took over a lease that had expired |
| `MultihubLeaseLost` | Warning | this hub's in-progress operation was stopped because another hub now holds the lease |
| `MultihubLeaseError` | Warning | the lease could not be read or updated (first occurrence of each distinct error) |
| `MultihubLeaseReleaseError` | Warning | the lease could not be released (it expires on its own) |
| `MultihubOperationWaiting` | Normal | an operation (e.g. a manual sync) is waiting for another hub's lease (once per wait) |
| `MultihubOverrideSet` | Normal | a user's sync to a specific revision on this hub recorded the override |
| `MultihubOverrideCleared` | Normal | a user's sync to the targetRevision on this hub cleared it |
| `MultihubAutoSyncPaused` | Normal | this hub's auto-sync stood down because of an override (once per change) |
| `MultihubOperationTakenOver` | Normal | this hub's in-progress operation was taken over by another hub; this hub now shows that hub's state |

A takeover by the claim path also raises `MultihubLeaseTakeover`, and the
operation carries `info: multihub=taken over from <hub>`. A failed mirror
(`mirror_error`) raises `MultihubLeaseError`.

Routine acquire/release has no Event. Upstream's `OperationStarted` /
`OperationCompleted` already cover operations.

### Metrics

`argocd_multihub_lease_decisions_total{hub, path, event}`: a counter on the
controller's existing `/metrics` endpoint. `path` is `auto-sync`,
`operation`, `pin`, `takeover`, `override` or `refresh`. Useful queries:

```promql
# takeovers, should be ~0 outside of hub failures
sum by (hub) (increase(argocd_multihub_lease_decisions_total{event="takeover"}[1h]))
# how often each hub is kept waiting by another
sum by (hub, path) (rate(argocd_multihub_lease_decisions_total{event=~"held|settling"}[5m]))
# lease errors (destination unreachable / RBAC)
sum by (hub) (rate(argocd_multihub_lease_decisions_total{event=~"error|release_error"}[5m]))
```

No per-app labels, to keep cardinality bounded. Per-app detail is in the
logs and Events.

### State on the destination

`kubectl -n argocd-coordination get lease <installationID>.<app> -o yaml`
shows:
- the current holder, `acquireTime`/`renewTime` and `leaseTransitions`;
- the `multihub.argoproj.io/released-by` / `released-at` annotations from
  the last release;
- the revision pin (`multihub.argoproj.io/pinned-revision`,
  `pinned-commit-time`, `pinned-by`, `pinned-at`). That's current state only, not history;
- the override (`multihub.argoproj.io/override-revision`, `override-by`,
  `override-at`, `override-reason`), if a rollback is in force. Delete those
  annotations to clear it by hand;
- the last full refresh (`multihub.argoproj.io/refreshed-by`, `-at`,
  `-revision`, `-fingerprint`, `-result-hash`), with the result itself in
  the ConfigMap `<lease name>-result` (gzipped);
- the mirrored operation (`multihub.argoproj.io/operation`):
  `base64(gzip(header JSON + "\n" + OperationState JSON))`, where the header
  is `{"hub","generation","running"}`. Capped at 200KiB. To decode it:
  `kubectl ... -o jsonpath='{.metadata.annotations.multihub\.argoproj\.io/operation}' | base64 -d | gunzip`
  (or `hack/multi-hub-lab/opstate.sh`).
