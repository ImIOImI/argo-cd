package controller

// Glue between the application controller and controller/multihub. Kept in its
// own file so the fork's footprint in appcontroller.go stays a few call sites.

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/argoproj/argo-cd/v3/controller/multihub"
	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	applog "github.com/argoproj/argo-cd/v3/util/app/log"
	"github.com/argoproj/argo-cd/v3/util/argo"
)

const (
	// How often a hub waiting for another hub's operation lease re-checks.
	multihubOperationWait = 5 * time.Second
	// Upper bound on the auto-sync re-check delay. The holder's own sync
	// changes the destination, which triggers a refresh sooner anyway.
	multihubMaxAutoSyncRecheck = 15 * time.Second
)

// Kubernetes Event reasons emitted on the Application.
const (
	EventReasonMultihubTakeover         = "MultihubLeaseTakeover"
	EventReasonMultihubLost             = "MultihubLeaseLost"
	EventReasonMultihubError            = "MultihubLeaseError"
	EventReasonMultihubReleaseError     = "MultihubLeaseReleaseError"
	EventReasonMultihubOperationWaiting = "MultihubOperationWaiting"
)

// multihubInit is called once from NewApplicationController.
func (ctrl *ApplicationController) multihubInit() {
	cfg := multihub.ConfigFromEnv()
	ctrl.multihub = multihub.New(cfg, ctrl.multihubClient)
	if !ctrl.multihub.Enabled() {
		return
	}
	t := cfg.Telemetry
	if t.Metrics {
		if err := multihub.RegisterMetrics(ctrlmetrics.Registry); err != nil {
			log.WithError(err).Warn("multihub: failed to register metrics")
		}
	}
	log.WithFields(log.Fields{
		"multihub.hub":            cfg.HubID,
		"multihub.namespace":      cfg.Namespace,
		"multihub.lease_duration": cfg.LeaseDuration.String(),
		"multihub.settle_window":  cfg.SettleWindow.String(),
		"multihub.log_lifecycle":  t.LogLifecycle,
		"multihub.log_renewals":   t.LogRenewals,
		"multihub.log_denials":    string(t.LogDenials),
		"multihub.events":         t.Events,
		"multihub.metrics":        t.Metrics,
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
