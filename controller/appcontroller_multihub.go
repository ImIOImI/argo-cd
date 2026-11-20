package controller

// Glue between the application controller and controller/multihub. Kept in its
// own file so the fork's footprint in appcontroller.go stays a few call sites.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

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
