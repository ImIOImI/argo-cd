package controller

// Glue between the application controller and controller/multihub. Kept in its
// own file so the fork's footprint in appcontroller.go stays a few call sites.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/argoproj/argo-cd/v3/common"
	"github.com/argoproj/argo-cd/v3/controller/multihub"
	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	applog "github.com/argoproj/argo-cd/v3/util/app/log"
	"github.com/argoproj/argo-cd/v3/util/argo"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
)

const (
	// How often a hub waiting for another hub's operation lease re-checks.
	multihubOperationWait = 5 * time.Second
	// Upper bound on the auto-sync re-check delay. The holder's own sync
	// changes the destination, which triggers a refresh sooner anyway.
	multihubMaxAutoSyncRecheck = 15 * time.Second
	// multihubClaimSlack is added to a lease's remaining time when scheduling
	// the refresh that may claim it, so the lease has surely expired.
	multihubClaimSlack = time.Second
)

// Kubernetes Event reasons emitted on the Application.
const (
	EventReasonMultihubTakeover           = "MultihubLeaseTakeover"
	EventReasonMultihubLost               = "MultihubLeaseLost"
	EventReasonMultihubError              = "MultihubLeaseError"
	EventReasonMultihubReleaseError       = "MultihubLeaseReleaseError"
	EventReasonMultihubOperationTakenOver = "MultihubOperationTakenOver"
	EventReasonMultihubOverrideSet        = "MultihubOverrideSet"
	EventReasonMultihubOverrideCleared    = "MultihubOverrideCleared"
	EventReasonMultihubAutoSyncPaused     = "MultihubAutoSyncPaused"
	EventReasonMultihubOperationWaiting   = "MultihubOperationWaiting"
)

// multihubInit is called once from NewApplicationController.
func (ctrl *ApplicationController) multihubInit(repoClientset apiclient.Clientset) {
	cfg := multihub.ConfigFromEnv()
	ctrl.multihub = multihub.New(cfg, ctrl.multihubClient)
	if !ctrl.multihub.Enabled() {
		return
	}
	ctrl.multihubCommitTime = ctrl.multihubRepoCommitTime(repoClientset)
	t := cfg.Telemetry
	if t.Metrics {
		if err := multihub.RegisterMetrics(ctrlmetrics.Registry); err != nil {
			log.WithError(err).Warn("multihub: failed to register metrics")
		}
	}
	log.WithFields(log.Fields{
		"multihub.hub":                     cfg.HubID,
		"multihub.namespace":               cfg.Namespace,
		"multihub.lease_duration":          cfg.LeaseDuration.String(),
		"multihub.settle_window":           cfg.SettleWindow.String(),
		"multihub.log_lifecycle":           t.LogLifecycle,
		"multihub.log_renewals":            t.LogRenewals,
		"multihub.log_denials":             string(t.LogDenials),
		"multihub.log_denials_reset_after": t.DenialsResetAfter.String(),
		"multihub.events":                  t.Events,
		"multihub.metrics":                 t.Metrics,
	}).Infof("multihub coordination enabled: hub id %q", cfg.HubID)
}

// multihubClient builds a client for a destination cluster from its stored
// credentials (the same ones the controller syncs with).
func (ctrl *ApplicationController) multihubClient(server string) (kubernetes.Interface, error) {
	cluster, err := ctrl.db.GetCluster(context.Background(), server)
	if err != nil {
		return nil, err
	}
	cfg, err := cluster.RESTConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func (ctrl *ApplicationController) multihubTarget(ctx context.Context, app *appv1.Application) (string, multihub.Key, error) {
	cluster, err := argo.GetDestinationCluster(ctx, app.Spec.Destination, ctrl.db)
	if err != nil {
		return "", multihub.Key{}, fmt.Errorf("multihub: destination cluster: %w", err)
	}
	installationID, err := ctrl.settingsMgr.GetInstallationID()
	if err != nil {
		return "", multihub.Key{}, fmt.Errorf("multihub: installation ID: %w", err)
	}
	return cluster.Server, multihub.Key{InstallationID: installationID, InstanceName: app.InstanceName(ctrl.namespace)}, nil
}

// multihubAcquire takes or renews the operation lease for app on the given
// code path (multihub.PathAutoSync applies the settle window) and reports the
// decision. On error the returned Decision has Event == EventError.
func (ctrl *ApplicationController) multihubAcquire(ctx context.Context, app *appv1.Application, path string) (multihub.Decision, error) {
	server, key, err := ctrl.multihubTarget(ctx, app)
	var d multihub.Decision
	if err == nil {
		if path == multihub.PathAutoSync {
			d, err = ctrl.multihub.AcquireForAutoSync(ctx, server, key)
		} else {
			d, err = ctrl.multihub.AcquireForOperation(ctx, server, key)
		}
	}
	if err != nil {
		d = multihub.Decision{Event: multihub.EventError, Reason: err.Error()}
	}
	ctrl.multihubReport(ctx, app, path, d, multihubMessage(path, d))
	return d, err
}

func (ctrl *ApplicationController) multihubRelease(ctx context.Context, app *appv1.Application) {
	if !ctrl.multihub.Enabled() {
		return
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err == nil {
		err = ctrl.multihub.Release(ctx, server, key)
	}
	d := multihub.Decision{Event: multihub.EventReleased, Holder: ctrl.multihub.HubID(), Reason: "operation completed"}
	if err != nil {
		// Not fatal: the lease expires on its own.
		d = multihub.Decision{Event: multihub.EventReleaseError, Reason: err.Error()}
	}
	ctrl.multihubReport(ctx, app, multihub.PathOperation, d, multihubMessage(multihub.PathOperation, d))
}

// multihubLost reports an in-progress operation that found its lease taken over.
func (ctrl *ApplicationController) multihubLost(ctx context.Context, app *appv1.Application, d multihub.Decision, message string) {
	d.Event = multihub.EventLost
	ctrl.multihubReport(ctx, app, multihub.PathOperation, d, message)
}

// multihubReport is the single place a lease decision becomes a log line, a
// metric sample and, by policy, a Kubernetes Event (see multihub/telemetry.go).
func (ctrl *ApplicationController) multihubReport(ctx context.Context, app *appv1.Application, path string, d multihub.Decision, message string) {
	if !ctrl.multihub.Enabled() || d.Event == multihub.EventDisabled {
		return
	}
	hub := ctrl.multihub.HubID()
	if ctrl.multihub.Telemetry().Metrics {
		multihub.DecisionsTotal.WithLabelValues(hub, path, string(d.Event)).Inc()
	}
	plan := ctrl.multihub.Reporter().Plan(app.QualifiedName(), path, d.Event, d.Holder, d.Reason)

	fields := log.Fields{
		"multihub.hub":    hub,
		"multihub.path":   path,
		"multihub.event":  string(d.Event),
		"multihub.reason": d.Reason,
	}
	if d.Holder != "" {
		fields["multihub.holder"] = d.Holder
	}
	if d.Generation != 0 {
		fields["multihub.generation"] = d.Generation
	}
	if d.RetryAfter > 0 {
		fields["multihub.retry_after"] = d.RetryAfter.Round(time.Millisecond).String()
	}
	log.WithFields(applog.GetAppLogFields(app)).WithFields(fields).Log(plan.LogLevel, message)

	if plan.Event {
		eventType := corev1.EventTypeNormal
		if plan.Warning {
			eventType = corev1.EventTypeWarning
		}
		ctrl.logAppEvent(ctx, app, argo.EventInfo{Type: eventType, Reason: multihubEventReason(d.Event)}, message)
	}
}

func multihubEventReason(ev multihub.Event) string {
	switch ev {
	case multihub.EventTakeover:
		return EventReasonMultihubTakeover
	case multihub.EventLost:
		return EventReasonMultihubLost
	case multihub.EventReleaseError:
		return EventReasonMultihubReleaseError
	case multihub.EventHeld, multihub.EventSettling:
		return EventReasonMultihubOperationWaiting
	case multihub.EventFollowing:
		return EventReasonMultihubOperationTakenOver
	default:
		return EventReasonMultihubError
	}
}

// multihubMessage is the human-readable log/Event message for a decision.
// Refusals keep upstream's "Skipping auto-sync: ..." phrasing so existing
// log searches still find them.
func multihubMessage(path string, d multihub.Decision) string {
	switch d.Event {
	case multihub.EventHeld, multihub.EventSettling:
		if path == multihub.PathAutoSync {
			return fmt.Sprintf("Skipping auto-sync: %s (rechecking in %v)", d.Reason, multihubRecheckDelay(d).Round(time.Millisecond))
		}
		return "Waiting to start operation: " + d.Reason
	case multihub.EventError:
		if path == multihub.PathAutoSync {
			return "Skipping auto-sync: multihub operation lease unavailable: " + d.Reason
		}
		return "Waiting to start operation: multihub operation lease unavailable: " + d.Reason
	case multihub.EventTakeover:
		return fmt.Sprintf("multihub: operation lease %s; took it over (generation %d)", d.Reason, d.Generation)
	case multihub.EventReleased:
		return "multihub: released operation lease"
	case multihub.EventReleaseError:
		return "multihub: failed to release operation lease (it will expire): " + d.Reason
	default:
		return fmt.Sprintf("multihub: operation lease %s (generation %d)", d.Reason, d.Generation)
	}
}

func multihubRecheckDelay(d multihub.Decision) time.Duration {
	if d.RetryAfter <= 0 || d.RetryAfter > multihubMaxAutoSyncRecheck {
		return multihubMaxAutoSyncRecheck
	}
	return d.RetryAfter
}

// multihubPinRevision agrees app's compared revision with the other hubs
// (revision pin, C1). It returns the revision to use and whether it differs
// from the one compared, i.e. whether the caller must compare again.
//
// It passes the compared revision through unchanged (no pin) when
// coordination is off, the revision is empty, the app has several sources
// (not pinned yet), the source isn't git (helm/OCI), no commit-time lookup
// is configured, or the commit time
// or the pin can't be read: a hub that can't agree behaves like a single hub.
func (ctrl *ApplicationController) multihubPinRevision(ctx context.Context, app *appv1.Application, compared string) (string, bool) {
	if !ctrl.multihub.Enabled() || compared == "" || app.Spec.HasMultipleSources() || ctrl.multihubCommitTime == nil {
		return compared, false
	}
	if src := app.Spec.GetSource(); src.IsHelm() || src.IsOCI() {
		return compared, false // commit time only orders git revisions
	}
	commitTime, err := ctrl.multihubCommitTime(ctx, app, compared)
	if err != nil {
		ctrl.multihubReportPin(app, compared, multihub.PinDecision{Event: multihub.PinError}, err.Error())
		return compared, false
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err != nil {
		ctrl.multihubReportPin(app, compared, multihub.PinDecision{Event: multihub.PinError}, err.Error())
		return compared, false
	}
	d, err := ctrl.multihub.AgreeRevision(ctx, server, key, multihub.Candidate{Revision: compared, CommitTime: commitTime})
	if err != nil || d.Pin.Revision == "" {
		reason := "empty pin"
		if err != nil {
			reason = err.Error()
		}
		ctrl.multihubReportPin(app, compared, multihub.PinDecision{Event: multihub.PinError}, reason)
		return compared, false
	}
	ctrl.multihubReportPin(app, compared, d, "")
	if d.Pin.Revision != compared {
		return d.Pin.Revision, true
	}
	return compared, false
}

// multihubReportPin logs and counts one revision-pin decision.
func (ctrl *ApplicationController) multihubReportPin(app *appv1.Application, candidate string, d multihub.PinDecision, reason string) {
	hub := ctrl.multihub.HubID()
	if ctrl.multihub.Telemetry().Metrics {
		multihub.DecisionsTotal.WithLabelValues(hub, multihub.PathPin, string(d.Event)).Inc()
	}
	plan := ctrl.multihub.Reporter().PlanPin(app.QualifiedName(), d.Event, reason)
	fields := log.Fields{
		"multihub.hub":                hub,
		"multihub.path":               multihub.PathPin,
		"multihub.event":              string(d.Event),
		"multihub.candidate_revision": candidate,
	}
	if d.Pin.Revision != "" {
		fields["multihub.pinned_revision"] = d.Pin.Revision
		fields["multihub.pinned_by"] = d.Pin.By
	}
	if reason != "" {
		fields["multihub.reason"] = reason
	}
	var msg string
	switch d.Event {
	case multihub.PinPinned:
		msg = "multihub: pinned revision " + d.Pin.Revision
	case multihub.PinAdvanced:
		msg = "multihub: advanced the pinned revision to " + d.Pin.Revision
	case multihub.PinAdopted:
		msg = fmt.Sprintf("multihub: using pinned revision %s (pinned by %s) instead of this hub's %s", d.Pin.Revision, d.Pin.By, candidate)
	case multihub.PinKept:
		msg = "multihub: revision " + candidate + " matches the pin"
	default:
		msg = fmt.Sprintf("multihub: revision pin unavailable, using this hub's revision %s: %s", candidate, reason)
	}
	log.WithFields(applog.GetAppLogFields(app)).WithFields(fields).Log(plan.LogLevel, msg)
}

// multihubRepoCommitTime looks a git revision's commit time up through the
// repo-server (revision metadata, cached there), like the hydrator does.
func (ctrl *ApplicationController) multihubRepoCommitTime(repoClientset apiclient.Clientset) func(context.Context, *appv1.Application, string) (time.Time, error) {
	return func(ctx context.Context, app *appv1.Application, revision string) (time.Time, error) {
		src := app.Spec.GetSource()
		repo, err := ctrl.db.GetRepository(ctx, src.RepoURL, app.Spec.Project)
		if err != nil {
			return time.Time{}, fmt.Errorf("multihub: repository %q: %w", src.RepoURL, err)
		}
		closer, client, err := repoClientset.NewRepoServerClient()
		if err != nil {
			return time.Time{}, fmt.Errorf("multihub: repo-server client: %w", err)
		}
		defer utilio.Close(closer)
		md, err := client.GetRevisionMetadata(ctx, &apiclient.RepoServerRevisionMetadataRequest{Repo: repo, Revision: revision})
		if err != nil {
			return time.Time{}, fmt.Errorf("multihub: revision metadata for %s: %w", revision, err)
		}
		if md == nil || md.Date == nil || md.Date.IsZero() {
			return time.Time{}, fmt.Errorf("multihub: revision %s has no commit date", revision)
		}
		return md.Date.Time, nil
	}
}

// multihubMirror records app's operation state on its Lease after a pass
// (C3), so another hub can take the operation over if this one dies.
func (ctrl *ApplicationController) multihubMirror(ctx context.Context, app *appv1.Application, generation int32, state *appv1.OperationState) {
	if !ctrl.multihub.Enabled() || state == nil {
		return
	}
	data, err := json.Marshal(state)
	if err == nil {
		var server string
		var key multihub.Key
		server, key, err = ctrl.multihubTarget(ctx, app)
		if err == nil {
			err = ctrl.multihub.MirrorOperation(ctx, server, key, generation, !state.Phase.Completed(), data)
		}
	}
	if err != nil {
		d := multihub.Decision{Event: multihub.EventMirrorError, Generation: generation, Reason: err.Error()}
		ctrl.multihubReport(ctx, app, multihub.PathOperation, d, "multihub: failed to mirror operation state (another hub taking over would restart it): "+err.Error())
	}
}

// multihubClaimOrphan takes over another hub's running operation on app once
// that hub's lease has expired (C3), and resumes it here: it sets the
// mirrored operation and operation state on this hub's Application and
// queues the operation. It reports whether app now has an operation.
func (ctrl *ApplicationController) multihubClaimOrphan(ctx context.Context, app *appv1.Application) bool {
	if !ctrl.multihub.Enabled() || app.Operation != nil {
		return false
	}
	if ok, _ := ctrl.multihubMayStart(ctx); !ok {
		return false
	}
	fail := func(err error) bool {
		d := multihub.Decision{Event: multihub.EventError, Reason: err.Error()}
		ctrl.multihubReport(ctx, app, multihub.PathTakeover, d, "multihub: could not take over another hub's operation: "+err.Error())
		return false
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err != nil {
		return fail(err)
	}
	claim, err := ctrl.multihub.ClaimOrphan(ctx, server, key)
	if err != nil {
		return fail(err)
	}
	if claim.Event == multihub.ClaimWaiting {
		// Look again when the other hub's lease expires: if it has died by
		// then, that refresh claims the operation.
		ctrl.multihubRequeue(app, claim.RetryAfter+multihubClaimSlack)
		return false
	}
	if claim.Event != multihub.ClaimClaimed {
		return false
	}
	var state appv1.OperationState
	if err := json.Unmarshal(claim.Mirror.State, &state); err != nil {
		return fail(fmt.Errorf("decode operation mirrored by %s: %w", claim.Mirror.Hub, err))
	}
	op := state.Operation.DeepCopy()
	op.Info = append(op.Info, &appv1.Info{Name: "multihub", Value: "taken over from " + claim.Mirror.Hub})
	state.Operation = *op
	// A JSON patch "add" replaces each value whole. A merge patch would keep
	// fields the mirror leaves out (e.g. finishedAt of this hub's previous
	// operation, which on a Running operation means "retry pending").
	patch, err := json.Marshal([]map[string]any{
		{"op": "add", "path": "/operation", "value": op},
		{"op": "add", "path": "/status/operationState", "value": state},
	})
	if err != nil {
		return fail(err)
	}
	if _, err := ctrl.PatchAppWithWriteBack(ctx, app.Name, app.Namespace, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		// Give the claim back, or the mirror would name this hub, which never
		// claims its own operations, and the operation would be stranded.
		if rerr := ctrl.multihub.ReturnClaim(ctx, server, key, claim); rerr != nil {
			err = fmt.Errorf("%w (and giving the claim back failed: %v)", err, rerr)
		}
		return fail(fmt.Errorf("set operation taken over from %s: %w", claim.Mirror.Hub, err))
	}
	app.Operation = op
	app.Status.OperationState = &state
	ctrl.appOperationQueue.Add(ctrl.toAppKey(app.QualifiedName()))

	d := multihub.Decision{Event: multihub.EventTakeover, Holder: claim.Mirror.Hub, Generation: claim.Generation, Reason: "lease held by " + claim.Mirror.Hub + " expired"}
	ctrl.multihubReport(ctx, app, multihub.PathTakeover, d, fmt.Sprintf(
		"multihub: took over the running operation from %s (its lease expired); resuming it from %q, started %s (generation %d)",
		claim.Mirror.Hub, state.Message, state.StartedAt.UTC().Format(time.RFC3339), claim.Generation))
	return true
}

// multihubTakenOver reports whether app's in-progress operation was taken
// over by another hub (C3): the mirror on the Lease is the same operation
// (same startedAt), recorded by another hub. It returns that hub's state.
func (ctrl *ApplicationController) multihubTakenOver(ctx context.Context, app *appv1.Application) (*appv1.OperationState, string, bool) {
	ours := app.Status.OperationState
	if !ctrl.multihub.Enabled() || ours == nil {
		return nil, "", false
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err != nil {
		return nil, "", false
	}
	m, ok, err := ctrl.multihub.CurrentOperation(ctx, server, key)
	if err != nil || !ok || m.Hub == ctrl.multihub.HubID() {
		return nil, "", false
	}
	var theirs appv1.OperationState
	if err := json.Unmarshal(m.State, &theirs); err != nil || !theirs.StartedAt.Equal(&ours.StartedAt) {
		return nil, "", false
	}
	return &theirs, m.Hub, true
}

func (ctrl *ApplicationController) multihubRequeue(app *appv1.Application, after time.Duration) {
	if ctrl.multihubRequeueRefresh != nil {
		ctrl.multihubRequeueRefresh(app, after)
		return
	}
	ctrl.requestAppRefresh(app.QualifiedName(), CompareWithRecent.Pointer(), &after)
}

// multihubShowState writes another hub's operation state onto app as-is
// (C3 follow). Unlike setOperationState it doesn't stamp finishedAt: the
// operation finished when the other hub says it did. A completed state
// also clears app's operation.
func (ctrl *ApplicationController) multihubShowState(ctx context.Context, app *appv1.Application, state *appv1.OperationState) {
	ops := []map[string]any{{"op": "add", "path": "/status/operationState", "value": state}}
	if state.Phase.Completed() && app.Operation != nil {
		ops = append(ops, map[string]any{"op": "remove", "path": "/operation"})
	}
	patch, err := json.Marshal(ops)
	if err == nil {
		_, err = ctrl.PatchAppWithWriteBack(ctx, app.Name, app.Namespace, types.JSONPatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		log.WithFields(applog.GetAppLogFields(app)).WithError(err).Warn("multihub: failed to show the operation state of the hub that took it over")
		return
	}
	app.Status.OperationState = state
	if state.Phase.Completed() {
		app.Operation = nil
	}
}

// multihubApplyOverride records or clears the C5 override for a new,
// coordinated operation: a user's sync to a specific revision (e.g. a
// rollback) records one, so every hub's auto-sync stands down; a user's
// sync to the app's targetRevision clears it. Automated and partial syncs
// leave it alone.
func (ctrl *ApplicationController) multihubApplyOverride(ctx context.Context, app *appv1.Application, op *appv1.Operation) error {
	if !ctrl.multihub.Enabled() || op == nil || op.Sync == nil || op.InitiatedBy.Automated ||
		op.Sync.DryRun || len(op.Sync.Resources) > 0 || app.Spec.HasMultipleSources() {
		return nil
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err != nil {
		return fmt.Errorf("multihub: could not record the override: %w", err)
	}
	who := op.InitiatedBy.Username
	if who == "" {
		who = "unknown user"
	}
	rev, target := op.Sync.Revision, app.Spec.GetSource().TargetRevision
	if rev == "" || rev == target || (target == "" && rev == "HEAD") {
		removed, ok, err := ctrl.multihub.ClearOverride(ctx, server, key)
		if err != nil {
			// Not fatal: auto-sync stays paused until the next clear.
			d := multihub.Decision{Event: multihub.EventError, Reason: err.Error()}
			ctrl.multihubReport(ctx, app, multihub.PathOverride, d, "multihub: could not clear the override: "+err.Error())
			return nil
		}
		if ok {
			ctrl.multihubReportOverride(ctx, app, multihub.EventOverrideCleared, removed, fmt.Sprintf(
				"multihub: override to %s (%s, set by %s) cleared by a sync to %s by %s; automated syncs resume on every hub",
				removed.Revision, removed.Reason, removed.By, orHEAD(target), who))
		}
		return nil
	}
	o, err := ctrl.multihub.SetOverride(ctx, server, key, rev, fmt.Sprintf("sync to %s by %s", rev, who))
	if err != nil {
		return fmt.Errorf("multihub: could not record the override for this sync to %s, so other hubs would revert it: %w", rev, err)
	}
	ctrl.multihubReportOverride(ctx, app, multihub.EventOverrideSet, o, fmt.Sprintf(
		"multihub: override to %s recorded (%s); automated syncs are paused on every hub until a sync to %s",
		o.Revision, o.Reason, orHEAD(target)))
	return nil
}

func orHEAD(rev string) string {
	if rev == "" {
		return "HEAD"
	}
	return rev
}

// multihubReportOverride logs, counts and (by policy) raises an Event for an
// override change.
func (ctrl *ApplicationController) multihubReportOverride(ctx context.Context, app *appv1.Application, ev multihub.Event, o multihub.Override, message string) {
	hub := ctrl.multihub.HubID()
	if ctrl.multihub.Telemetry().Metrics {
		multihub.DecisionsTotal.WithLabelValues(hub, multihub.PathOverride, string(ev)).Inc()
	}
	plan := ctrl.multihub.Reporter().Plan(app.QualifiedName(), multihub.PathOverride, ev, o.By, o.Reason)
	log.WithFields(applog.GetAppLogFields(app)).WithFields(log.Fields{
		"multihub.hub":               hub,
		"multihub.path":              multihub.PathOverride,
		"multihub.event":             string(ev),
		"multihub.override_revision": o.Revision,
		"multihub.override_by":       o.By,
		"multihub.reason":            o.Reason,
	}).Log(plan.LogLevel, message)
	if plan.Event {
		reason := EventReasonMultihubOverrideSet
		switch ev {
		case multihub.EventOverrideCleared:
			reason = EventReasonMultihubOverrideCleared
		case multihub.EventOverridden:
			reason = EventReasonMultihubAutoSyncPaused
		}
		ctrl.logAppEvent(ctx, app, argo.EventInfo{Type: corev1.EventTypeNormal, Reason: reason}, message)
	}
}

// multihubOverridden reports whether an override (C5) means this hub's
// auto-sync must stand down, and says so (info when it changes). If the
// override can't be read, auto-sync also stands down: the same destination
// would refuse the operation lease anyway.
func (ctrl *ApplicationController) multihubOverridden(ctx context.Context, app *appv1.Application) bool {
	if !ctrl.multihub.Enabled() {
		return false
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	var o multihub.Override
	var ok bool
	if err == nil {
		o, ok, err = ctrl.multihub.CurrentOverride(ctx, server, key)
	}
	if err != nil {
		d := multihub.Decision{Event: multihub.EventError, Reason: err.Error()}
		ctrl.multihubReport(ctx, app, multihub.PathAutoSync, d, "Skipping auto-sync: multihub override unavailable: "+err.Error())
		return true
	}
	if !ok {
		return false
	}
	ctrl.multihubReportOverride(ctx, app, multihub.EventOverridden, o, fmt.Sprintf(
		"Skipping auto-sync: override to %s in place (%s, recorded by %s); sync to %s on any hub to resume automated syncs",
		o.Revision, o.Reason, o.By, orHEAD(app.Spec.GetSource().TargetRevision)))
	return true
}

// multihubSharedResult is what a hub publishes after a full refresh (C7):
// everything another hub needs to show the same status without comparing.
type multihubSharedResult struct {
	Sync               appv1.SyncStatus              `json:"sync"`
	Health             appv1.AppHealthStatus         `json:"health"`
	Resources          []appv1.ResourceStatus        `json:"resources,omitempty"`
	SourceType         appv1.ApplicationSourceType   `json:"sourceType,omitempty"`
	SourceTypes        []appv1.ApplicationSourceType `json:"sourceTypes,omitempty"`
	HasPreDeleteHooks  bool                          `json:"hasPreDeleteHooks,omitempty"`
	HasPostDeleteHooks bool                          `json:"hasPostDeleteHooks,omitempty"`
}

// multihubFingerprint hashes the parts of app's definition that decide what
// gets deployed (C7/C9), with the destination resolved to server. Auto-sync
// settings, labels and annotations are left out: they may differ per hub.
func multihubFingerprint(app *appv1.Application, server string) string {
	var syncOptions appv1.SyncOptions
	var managedNS *appv1.ManagedNamespaceMetadata
	if p := app.Spec.SyncPolicy; p != nil {
		syncOptions, managedNS = p.SyncOptions, p.ManagedNamespaceMetadata
	}
	def := struct {
		Source            *appv1.ApplicationSource          `json:"source,omitempty"`
		Sources           appv1.ApplicationSources          `json:"sources,omitempty"`
		Server            string                            `json:"server"`
		Namespace         string                            `json:"namespace"`
		Project           string                            `json:"project"`
		SyncOptions       appv1.SyncOptions                 `json:"syncOptions,omitempty"`
		ManagedNamespace  *appv1.ManagedNamespaceMetadata   `json:"managedNamespaceMetadata,omitempty"`
		IgnoreDifferences []appv1.ResourceIgnoreDifferences `json:"ignoreDifferences,omitempty"`
	}{app.Spec.Source, app.Spec.Sources, server, app.Spec.Destination.Namespace, app.Spec.Project, syncOptions, managedNS, app.Spec.IgnoreDifferences}
	raw, _ := json.Marshal(def)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// multihubShareable: C7 shares refreshes only where C1 can pin a revision
// (single-source git apps), since a shared result is only valid at the
// agreed revision.
func (ctrl *ApplicationController) multihubShareable(app *appv1.Application) bool {
	if !ctrl.multihub.Enabled() || app.Spec.HasMultipleSources() || app.Spec.Source == nil {
		return false
	}
	src := app.Spec.GetSource()
	return !src.IsHelm() && !src.IsOCI()
}

// multihubPublishRefresh publishes this hub's full refresh of app (C7).
// Failures are logged at debug: the other hub then simply refreshes itself.
func (ctrl *ApplicationController) multihubPublishRefresh(ctx context.Context, app *appv1.Application, result multihubSharedResult) {
	if !ctrl.multihubShareable(app) || result.Sync.Revision == "" {
		return
	}
	if ok, _ := ctrl.multihubMayStart(ctx); !ok {
		return
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err == nil {
		var raw []byte
		if raw, err = json.Marshal(result); err == nil {
			_, err = ctrl.multihub.PublishRefresh(ctx, server, key, result.Sync.Revision, multihubFingerprint(app, server), raw)
		}
	}
	if err != nil {
		log.WithFields(applog.GetAppLogFields(app)).WithError(err).Debug("multihub: failed to publish refresh")
	}
}

func multihubHasErrorCondition(app *appv1.Application) bool {
	for i := range app.Status.Conditions {
		if app.Status.Conditions[i].IsError() {
			return true
		}
	}
	return false
}

// multihubSkipRefresh applies another hub's fresh, published refresh of app
// instead of comparing (C7). It reports whether it did; if not, the caller
// refreshes normally.
func (ctrl *ApplicationController) multihubSkipRefresh(ctx context.Context, app *appv1.Application) bool {
	if !ctrl.multihub.ShareRefresh() || !ctrl.multihubShareable(app) || !ctrl.multihubPeriodicRefreshOnly(app) {
		return false
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err != nil {
		return false
	}
	view, ok := ctrl.multihub.Cached(server, key)
	rec := view.Refresh
	if !ok || !view.HasRefresh || !view.HasPin ||
		rec.By == ctrl.multihub.HubID() ||
		time.Since(rec.At) >= ctrl.refreshPeriod() ||
		rec.Revision != view.Pin.Revision ||
		rec.Fingerprint != multihubFingerprint(app, server) {
		return false
	}
	raw, err := ctrl.multihub.FetchResult(ctx, server, key, rec.ResultHash)
	if err != nil {
		return false
	}
	var res multihubSharedResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return false
	}
	// Delete-hook finalizers follow the comparison; let a local refresh
	// handle any change to them.
	if res.HasPreDeleteHooks != app.HasPreDeleteFinalizer() || res.HasPostDeleteHooks != app.HasPostDeleteFinalizer() {
		return false
	}
	app.Status.Sync = res.Sync
	app.Status.Health = res.Health
	app.Status.Resources = res.Resources
	app.Status.SourceType = res.SourceType
	app.Status.SourceTypes = res.SourceTypes
	app.Status.ControllerNamespace = ctrl.namespace
	// The resource tree comes from this hub's own live watch of the spoke:
	// rebuild it, as upstream's status-only refresh does.
	if destCluster, err := argo.GetDestinationCluster(ctx, app.Spec.Destination, ctrl.db); err == nil {
		managed := make([]*appv1.ResourceDiff, 0)
		instance := app.InstanceName(ctrl.namespace)
		if err := ctrl.cache.GetAppManagedResources(instance, &managed); err == nil {
			if tree, err := ctrl.getResourceTree(destCluster, app, managed); err == nil {
				app.Status.Summary = tree.GetSummary(app)
				_ = ctrl.cache.SetAppResourcesTree(instance, tree)
			}
		}
	}
	now := metav1.Now()
	ctrl.noteReconciled(app.QualifiedName(), now.Time)
	app.Status.ReconciledAt = &now
	if ctrl.multihub.Telemetry().Metrics {
		multihub.DecisionsTotal.WithLabelValues(ctrl.multihub.HubID(), multihub.PathRefresh, string(multihub.EventShared)).Inc()
	}
	log.WithFields(applog.GetAppLogFields(app)).WithFields(log.Fields{
		"multihub.hub":      ctrl.multihub.HubID(),
		"multihub.path":     multihub.PathRefresh,
		"multihub.event":    string(multihub.EventShared),
		"multihub.holder":   rec.By,
		"multihub.revision": rec.Revision,
	}).Debugf("multihub: periodic refresh skipped; using %s's refresh from %s ago", rec.By, time.Since(rec.At).Round(time.Second))
	return true
}

// multihubPeriodicRefreshOnly reports whether app is due only because its
// periodic (soft) refresh period ran out: no user or controller request, no
// spec change, no hard refresh. Only those refreshes are shared (C7).
func (ctrl *ApplicationController) multihubPeriodicRefreshOnly(app *appv1.Application) bool {
	if _, ok := app.IsRefreshRequested(); ok {
		return false
	}
	if !currentSourceEqualsSyncedSource(app) {
		return false
	}
	soft, hard := comparisonExpiry(ctrl.statusWithEffectiveReconciledAt(app), ctrl.refreshPeriod(), ctrl.statusHardRefreshTimeout)
	if !soft || hard {
		return false
	}
	// Peek, don't consume: a pending controller request (e.g. a resource
	// changed) must still be refreshed locally.
	ctrl.refreshRequestedAppsMutex.Lock()
	_, pending := ctrl.refreshRequestedApps[app.QualifiedName()]
	ctrl.refreshRequestedAppsMutex.Unlock()
	return !pending
}

// refreshPeriod is the periodic refresh period in force now. With multihub
// on, it follows argocd-cm's timeout.reconciliation live, so one change to
// that key (e.g. from Git) changes every hub without a restart. Otherwise,
// or if the key is missing or invalid, it's the value the controller
// started with.
func (ctrl *ApplicationController) refreshPeriod() time.Duration {
	if !ctrl.multihub.Enabled() {
		return ctrl.statusRefreshTimeout
	}
	if m, ok := ctrl.multihubMembership.Current(); ok && m.RefreshPeriod > 0 {
		return m.RefreshPeriod
	}
	cm, err := ctrl.settingsMgr.GetConfigMapByName(common.ArgoCDConfigMapName)
	if err != nil {
		return ctrl.statusRefreshTimeout
	}
	d, err := time.ParseDuration(cm.Data[multihubRefreshPeriodKey])
	if err != nil || d < 0 {
		return ctrl.statusRefreshTimeout
	}
	return d
}

// multihubRefreshPeriodKey is the argocd-cm key upstream reads once at
// startup (as ARGOCD_RECONCILIATION_TIMEOUT); multihub reads it live.
const multihubRefreshPeriodKey = "timeout.reconciliation"

// multihubResyncLoop re-checks every app once per refresh period, with the
// period re-read each time, so lowering timeout.reconciliation live takes
// effect (the app informer's own resync period is fixed at startup). Each
// app is queued as upstream's informer resync queues it; apps not due are
// dismissed by the usual expiry check.
func (ctrl *ApplicationController) multihubResyncLoop(ctx context.Context) {
	wait := ctrl.multihubWait
	if wait == nil {
		wait = func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
	}
	for {
		d := ctrl.refreshPeriod()
		if d <= 0 {
			d = time.Minute // periodic refresh disabled: just look again later
		}
		if !wait(ctx, d) {
			return
		}
		if ctrl.refreshPeriod() <= 0 {
			continue
		}
		for _, obj := range ctrl.appInformer.GetIndexer().List() {
			app, ok := obj.(*appv1.Application)
			if !ok {
				continue
			}
			var delay *time.Duration
			if ctrl.statusRefreshJitter > 0 {
				j := time.Duration(float64(ctrl.statusRefreshJitter) * rand.Float64())
				delay = &j
			}
			ctrl.requestAppRefresh(app.QualifiedName(), nil, delay)
		}
	}
}

// MultihubWatchMembership starts following the HubMembership in the
// controller's namespace (C8/C9 groundwork). Called by the command with a
// dynamic client; a no-op when multihub is off.
func (ctrl *ApplicationController) MultihubWatchMembership(client dynamic.Interface) {
	if !ctrl.multihub.Enabled() || client == nil {
		return
	}
	ctrl.multihubMembership = multihub.NewMembershipWatcher(client, ctrl.namespace)
}

type multihubOwn struct {
	state  multihub.MemberState
	member bool
}

// multihubSelfServer is how a hub registers its own cluster.
const multihubSelfServer = "https://kubernetes.default.svc"

// multihubOwnState is this hub's state under the declared membership.
// declared is false when no HubMembership exists: today's behavior, every
// hub acts. member is false when this hub matches no entry. A membership
// that can't be resolved for this hub (conflicting selectors) keeps the
// state it had before, or watching if it never had one.
func (ctrl *ApplicationController) multihubOwnState(ctx context.Context) (state multihub.MemberState, member, declared bool) {
	m, ok := ctrl.multihubMembership.Current()
	if !ok {
		return multihub.StateActive, true, false
	}
	st, member, err := m.StateOf(ctrl.multihub.HubID(), ctrl.multihubSelfLabels(ctx))
	ctrl.multihubOwnMu.Lock()
	defer ctrl.multihubOwnMu.Unlock()
	if err != nil {
		log.WithError(err).Warn("multihub: can't resolve this hub's membership; keeping its previous state")
		if ctrl.multihubLastOwn != nil {
			return ctrl.multihubLastOwn.state, ctrl.multihubLastOwn.member, true
		}
		return multihub.StateWatching, true, true
	}
	ctrl.multihubLastOwn = &multihubOwn{state: st, member: member}
	return st, member, true
}

// multihubHeartbeatInterval is how often a hub refreshes its hub records.
const multihubHeartbeatInterval = 30 * time.Second

// multihubHeartbeat publishes this hub's record (id, self-registration
// labels, now) on every cluster it manages. Every multihub hub publishes,
// member or not: selectors can only match labels a hub has published.
func (ctrl *ApplicationController) multihubHeartbeat(ctx context.Context) {
	if !ctrl.multihub.Enabled() {
		return
	}
	lbls := ctrl.multihubSelfLabels(ctx)
	clusters, err := ctrl.db.ListClusters(ctx)
	if err != nil {
		log.WithError(err).Debug("multihub: heartbeat: can't list clusters")
		return
	}
	for _, c := range clusters.Items {
		if err := ctrl.multihub.PublishHubRecord(ctx, c.Server, lbls); err != nil {
			log.WithError(err).WithField("server", c.Server).Debug("multihub: heartbeat: can't publish hub record")
		}
	}
}

// multihubSelfLabels are the labels on this hub's own cluster
// self-registration, without Argo's bookkeeping label.
func (ctrl *ApplicationController) multihubSelfLabels(ctx context.Context) map[string]string {
	self, err := ctrl.db.GetCluster(ctx, multihubSelfServer)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range self.Labels {
		if k != common.LabelKeySecretType {
			out[k] = v
		}
	}
	return out
}

// multihubHeartbeatLoop runs multihubHeartbeat every interval until ctx ends.
func (ctrl *ApplicationController) multihubHeartbeatLoop(ctx context.Context) {
	wait := ctrl.multihubWait
	if wait == nil {
		wait = multihubTimerWait
	}
	for {
		ctrl.multihubHeartbeat(ctx)
		if !wait(ctx, multihubHeartbeatInterval) {
			return
		}
	}
}

func multihubTimerWait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// multihubMayStart reports whether this hub may start new work (syncs,
// takeovers, publishing shared refreshes) for shared apps, and if not, why.
// Only active members (or any hub, when no membership is declared) may.
func (ctrl *ApplicationController) multihubMayStart(ctx context.Context) (bool, string) {
	if !ctrl.multihub.Enabled() {
		return true, ""
	}
	st, member, declared := ctrl.multihubOwnState(ctx)
	switch {
	case !declared:
		return true, ""
	case !member:
		return false, fmt.Sprintf("hub %s is not a member of the hub group (HubMembership %s/%s); it starts no syncs", ctrl.multihub.HubID(), ctrl.namespace, multihub.HubMembershipName)
	case st == multihub.StateWatching:
		return false, fmt.Sprintf("hub %s is watching (HubMembership %s/%s); syncs run on the active hubs", ctrl.multihub.HubID(), ctrl.namespace, multihub.HubMembershipName)
	}
	return true, ""
}
