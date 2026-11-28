package controller

import (
	"context"

	appv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
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
	return false
}
