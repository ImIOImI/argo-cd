package controller

import (
	"context"

	log "github.com/sirupsen/logrus"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	applog "github.com/argoproj/argo-cd/v3/util/app/log"
)

// changedSinceLastSync works around argoproj/argo-cd#27875 (research R4,
// research/2026-11-28-r4-skipped-hooks.md). Auto-sync decides whether a new
// revision has changes against app.Status.Sync.Revision, the last revision
// *compared*. If a refresh compared desired without syncing it, the next
// refresh finds "no changes" and auto-sync treats desired as already
// attempted: a partial self-heal (hooks skipped) or, without self-heal, no
// sync at all. This re-checks against the last revision *synced*. It reports
// true when desired differs from the last synced revision and the app's
// source changed between them, or when that can't be determined (a full
// sync is the safe outcome). Remove when upstream fixes #27875.
func (ctrl *ApplicationController) changedSinceLastSync(ctx context.Context, app *appv1.Application, desired string) bool {
	op := app.Status.OperationState
	if op == nil || op.SyncResult == nil || app.Spec.HasMultipleSources() || app.Spec.Source == nil {
		return false // nothing to re-check against (multi-source: not covered)
	}
	last := op.SyncResult.Revision
	if last == "" || last == desired {
		return false
	}
	logCtx := log.WithFields(applog.GetAppLogFields(app))
	proj, err := ctrl.getAppProj(app)
	if err != nil {
		logCtx.WithError(err).Warn("Can't re-check changes since the last synced revision; treating them as changed")
		return true
	}
	// Ask the same question change detection asks, but from the last synced
	// revision instead of the last compared one.
	base := app.DeepCopy()
	base.Status.Sync.Revision = last
	changed, _, err := ctrl.appStateManager.EvaluateAppRevisionsChanges(ctx, base, []appv1.ApplicationSource{*base.Spec.Source}, []string{desired}, proj, false, false)
	if err != nil {
		logCtx.WithError(err).Warn("Can't re-check changes since the last synced revision; treating them as changed")
		return true
	}
	if changed {
		logCtx.Infof("Revision %s has changes since the last synced revision %s (argoproj/argo-cd#27875): a full sync, not already attempted", desired, last)
	}
	return changed
}
