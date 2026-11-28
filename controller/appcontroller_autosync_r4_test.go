package controller

import (
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/test"
)

const (
	r4Synced   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r4Compared = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// r4App: last synced at r4Synced; a later refresh compared r4Compared (a new
// commit) but didn't sync it, so Status.Sync.Revision is already r4Compared.
func r4App(selfHeal bool) *v1alpha1.Application {
	app := newFakeApp()
	app.Annotations = map[string]string{v1alpha1.AnnotationKeyManifestGeneratePaths: "."}
	app.Spec.SyncPolicy.Automated = &v1alpha1.SyncPolicyAutomated{SelfHeal: &selfHeal}
	finished := metav1.NewTime(time.Now().Add(-time.Hour))
	app.Status.OperationState = &v1alpha1.OperationState{
		Operation:  v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{Revision: r4Synced}},
		Phase:      common.OperationSucceeded,
		FinishedAt: &finished,
		SyncResult: &v1alpha1.SyncOperationResult{Revision: r4Synced, Source: *app.Spec.Source},
	}
	app.Status.Sync = v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeOutOfSync, Revision: r4Compared}
	return app
}

// r4AutoSync runs auto-sync with shouldCompareRevisions=false, which is
// what change detection reports when it compares r4Compared with itself;
// pathsChanged is the repo-server's answer for r4Synced..r4Compared.
func r4AutoSync(t *testing.T, app *v1alpha1.Application, pathsChanged bool) *v1alpha1.Application {
	t.Helper()
	ctrl := newFakeController(t.Context(), &fakeData{
		apps:                           []runtime.Object{app, &defaultProj},
		updateRevisionForPathsResponse: &apiclient.UpdateRevisionForPathsResponse{Changes: pathsChanged, Revision: r4Compared},
	}, nil)
	syncStatus := v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeOutOfSync, Revision: r4Compared}
	resources := []v1alpha1.ResourceStatus{{Name: "guestbook", Kind: kube.DeploymentKind, Status: v1alpha1.SyncStatusCodeOutOfSync}}
	_, _ = ctrl.autoSync(t.Context(), app, &syncStatus, resources, false)
	got, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications(test.FakeArgoCDNamespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

// The R4 / #27875 bug with self-heal: today a partial sync (hooks skipped).
func TestAutoSyncR4NewCommitComparedButNotSyncedIsAFullSync(t *testing.T) {
	got := r4AutoSync(t, r4App(true), true)
	require.NotNil(t, got.Operation, "a sync is started")
	assert.Equal(t, r4Compared, got.Operation.Sync.Revision)
	assert.Empty(t, got.Operation.Sync.Resources, "a full sync (hooks run), not a partial self-heal")
}

// The #27875 symptom without self-heal: today the commit isn't synced at all.
func TestAutoSyncR4WithoutSelfHealTheNewCommitIsStillSynced(t *testing.T) {
	got := r4AutoSync(t, r4App(false), true)
	require.NotNil(t, got.Operation, "not skipped as already attempted")
	assert.Equal(t, r4Compared, got.Operation.Sync.Revision)
	assert.Empty(t, got.Operation.Sync.Resources)
}

// Upstream's optimization is kept: if the app's paths really didn't change
// since the last sync, OutOfSync is drift, and self-heal is partial.
func TestAutoSyncR4NoPathChangesSinceTheLastSyncStaysASelfHeal(t *testing.T) {
	got := r4AutoSync(t, r4App(true), false)
	require.NotNil(t, got.Operation)
	assert.NotEmpty(t, got.Operation.Sync.Resources, "partial self-heal of the drifted resources")
}

// Desired is the revision already synced: behavior unchanged (self-heal).
func TestAutoSyncR4AlreadySyncedRevisionIsUnchanged(t *testing.T) {
	app := r4App(true)
	app.Status.OperationState.SyncResult.Revision = r4Compared
	app.Status.OperationState.Operation.Sync.Revision = r4Compared
	got := r4AutoSync(t, app, true)
	require.NotNil(t, got.Operation)
	assert.NotEmpty(t, got.Operation.Sync.Resources, "a self-heal of drift, as before")
}
