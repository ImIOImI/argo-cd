package controller

// Glue between the application controller and controller/multihub. Kept in its
// own file so the fork's footprint in appcontroller.go stays a few call sites.

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"

	"github.com/argoproj/argo-cd/v3/controller/multihub"
	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/argo"
)

const (
	// How often a hub waiting for another hub's operation lease re-checks.
	multihubOperationWait = 5 * time.Second
	// Upper bound on the auto-sync re-check delay. The holder's own sync
	// changes the destination, which triggers a refresh sooner anyway.
	multihubMaxAutoSyncRecheck = 15 * time.Second
)

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

// multihubAcquire takes or renews the operation lease for app. autoSync
// selects the settle-window variant.
func (ctrl *ApplicationController) multihubAcquire(ctx context.Context, app *appv1.Application, autoSync bool) (multihub.Decision, error) {
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err != nil {
		return multihub.Decision{}, err
	}
	if autoSync {
		return ctrl.multihub.AcquireForAutoSync(ctx, server, key)
	}
	return ctrl.multihub.AcquireForOperation(ctx, server, key)
}

func (ctrl *ApplicationController) multihubRelease(ctx context.Context, app *appv1.Application) {
	if !ctrl.multihub.Enabled() {
		return
	}
	server, key, err := ctrl.multihubTarget(ctx, app)
	if err == nil {
		err = ctrl.multihub.Release(ctx, server, key)
	}
	if err != nil {
		// Not fatal: the lease expires on its own.
		log.WithField("application", app.QualifiedName()).WithError(err).Warn("multihub: failed to release operation lease")
	}
}

func multihubRecheckDelay(d multihub.Decision) time.Duration {
	if d.RetryAfter <= 0 || d.RetryAfter > multihubMaxAutoSyncRecheck {
		return multihubMaxAutoSyncRecheck
	}
	return d.RetryAfter
}
