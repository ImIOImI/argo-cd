package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	synccommon "github.com/argoproj/argo-cd/gitops-engine/v3/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"

	"github.com/argoproj/argo-cd/v3/controller/multihub"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned/fake"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	mockrepoclient "github.com/argoproj/argo-cd/v3/reposerver/apiclient/mocks"
	"github.com/argoproj/argo-cd/v3/test"
)

var (
	pinT1 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	pinT2 = pinT1.Add(time.Minute)
)

// multihubPinFixture: a fake controller acting as hub-b, a fake spoke shared
// with a hub-a coordinator, and a fake commit-time lookup.
type multihubPinFixture struct {
	ctrl   *ApplicationController
	hubA   *multihub.Coordinator
	spoke  *k8sfake.Clientset
	app    *v1alpha1.Application
	lookup map[string]time.Time
	calls  int
}

func newMultihubPinFixture(t *testing.T, data *fakeData, app *v1alpha1.Application) *multihubPinFixture {
	t.Helper()
	ctrl := newFakeController(t.Context(), data, nil)
	f := &multihubPinFixture{
		ctrl:   ctrl,
		spoke:  k8sfake.NewClientset(),
		app:    app,
		lookup: map[string]time.Time{"old111": pinT1, "new222": pinT2},
	}
	clients := func(string) (kubernetes.Interface, error) { return f.spoke, nil }
	cfg := multihub.Config{LeaseDuration: time.Minute, Telemetry: multihub.DefaultTelemetry()}
	cfg.HubID = "hub-a"
	f.hubA = multihub.New(cfg, clients)
	cfg.HubID = "hub-b"
	ctrl.multihub = multihub.New(cfg, clients)
	ctrl.multihubCommitTime = func(_ context.Context, _ *v1alpha1.Application, revision string) (time.Time, error) {
		f.calls++
		if ts, ok := f.lookup[revision]; ok {
			return ts, nil
		}
		return time.Time{}, errors.New("unknown revision " + revision)
	}
	return f
}

// target is the destination server and key the controller itself resolves
// for the app, so the fixture and the code under test always agree.
func (f *multihubPinFixture) target(t *testing.T) (string, multihub.Key) {
	t.Helper()
	server, key, err := f.ctrl.multihubTarget(t.Context(), f.app)
	require.NoError(t, err)
	return server, key
}

func (f *multihubPinFixture) pinFromHubA(t *testing.T, rev string, at time.Time) {
	t.Helper()
	server, key := f.target(t)
	_, err := f.hubA.AgreeRevision(t.Context(), server, key, multihub.Candidate{Revision: rev, CommitTime: at})
	require.NoError(t, err)
}

func (f *multihubPinFixture) pin(t *testing.T) (multihub.Pin, bool) {
	t.Helper()
	server, key := f.target(t)
	p, ok, err := f.ctrl.multihub.CurrentPin(t.Context(), server, key)
	require.NoError(t, err)
	return p, ok
}

func pinTestApp() *v1alpha1.Application {
	app := newFakeApp()
	app.Spec.SyncPolicy = nil
	return app
}

func TestMultihubPinRevisionDisabledPassesThrough(t *testing.T) {
	app := pinTestApp()
	ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app, &defaultProj}}, nil)
	got, recompare := ctrl.multihubPinRevision(t.Context(), app, "old111")
	assert.Equal(t, "old111", got)
	assert.False(t, recompare)
}

func TestMultihubPinRevisionFirstHubPinsItsRevision(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)

	got, recompare := f.ctrl.multihubPinRevision(t.Context(), app, "new222")
	assert.Equal(t, "new222", got)
	assert.False(t, recompare)
	p, ok := f.pin(t)
	require.True(t, ok)
	assert.Equal(t, "new222", p.Revision)
	assert.Equal(t, "hub-b", p.By)
	assert.True(t, p.CommitTime.Equal(pinT2), "pin carries the commit time from the lookup")
}

// The E2 flap, unit-sized: hub-a pinned the new commit; this hub (hub-b)
// compared at its old one. It must switch to the pin and re-compare.
func TestMultihubPinRevisionStaleHubAdoptsPin(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	f.pinFromHubA(t, "new222", pinT2)

	got, recompare := f.ctrl.multihubPinRevision(t.Context(), app, "old111")
	assert.Equal(t, "new222", got)
	assert.True(t, recompare)
	p, _ := f.pin(t)
	assert.Equal(t, "new222", p.Revision, "a stale hub must not move the pin back")
	assert.Equal(t, "hub-a", p.By)
}

func TestMultihubPinRevisionNewerHubAdvancesPin(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	f.pinFromHubA(t, "old111", pinT1)

	got, recompare := f.ctrl.multihubPinRevision(t.Context(), app, "new222")
	assert.Equal(t, "new222", got)
	assert.False(t, recompare)
	p, _ := f.pin(t)
	assert.Equal(t, "new222", p.Revision)
	assert.Equal(t, "hub-b", p.By)
}

func TestMultihubPinRevisionSkipsMultiSourceApps(t *testing.T) {
	app := pinTestApp()
	src := *app.Spec.Source
	app.Spec.Source = nil
	app.Spec.Sources = v1alpha1.ApplicationSources{src, src}
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)

	got, recompare := f.ctrl.multihubPinRevision(t.Context(), app, "old111")
	assert.Equal(t, "old111", got)
	assert.False(t, recompare)
	_, ok := f.pin(t)
	assert.False(t, ok, "multi-source apps are not pinned yet")
	assert.Equal(t, 0, f.calls)
}

func TestMultihubPinRevisionCommitTimeErrorPassesThrough(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)

	got, recompare := f.ctrl.multihubPinRevision(t.Context(), app, "unknown999")
	assert.Equal(t, "unknown999", got)
	assert.False(t, recompare)
	_, ok := f.pin(t)
	assert.False(t, ok, "never pin without a commit time")
	assert.Equal(t, 1, f.calls, "the lookup was attempted")
}

func TestMultihubPinRevisionEmptyRevisionPassesThrough(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	got, recompare := f.ctrl.multihubPinRevision(t.Context(), app, "")
	assert.Empty(t, got)
	assert.False(t, recompare)
	assert.Equal(t, 0, f.calls)
}

// End to end through the refresh loop: a stale hub's refresh compares once at
// its own resolution, sees a newer pin, compares again at the pin, and
// persists the pin as the app's sync revision.
func TestMultihubRefreshComparesAtPin(t *testing.T) {
	app := pinTestApp()
	var requested []string
	data := &fakeData{
		apps: []runtime.Object{app, &defaultProj},
		manifestResponses: []*apiclient.ManifestResponse{
			{Manifests: []string{}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "old111"},
			{Manifests: []string{}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "new222"},
		},
		managedLiveObjs: map[kube.ResourceKey]*unstructured.Unstructured{},
		onGenerateManifest: func(req *apiclient.ManifestRequest) {
			requested = append(requested, req.Revision)
		},
	}
	f := newMultihubPinFixture(t, data, app)
	f.pinFromHubA(t, "new222", pinT2)

	key, err := cache.MetaNamespaceKeyFunc(app)
	require.NoError(t, err)
	f.ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatest.Pointer(), nil)
	f.ctrl.appRefreshQueue.Add(key)
	f.ctrl.processAppRefreshQueueItem()

	require.Len(t, requested, 2, "expected a second comparison at the pin")
	assert.Equal(t, "new222", requested[1])

	updated, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "new222", updated.Status.Sync.Revision)
}

func revisionMetadataClient(t *testing.T, md *v1alpha1.RevisionMetadata, err error) (*mockrepoclient.Clientset, *[]*apiclient.RepoServerRevisionMetadataRequest) {
	t.Helper()
	var seen []*apiclient.RepoServerRevisionMetadataRequest
	client := &mockrepoclient.RepoServerServiceClient{}
	client.EXPECT().GetRevisionMetadata(mock.Anything, mock.Anything).
		Run(func(_ context.Context, req *apiclient.RepoServerRevisionMetadataRequest, _ ...grpc.CallOption) {
			seen = append(seen, req)
		}).
		Return(md, err).Maybe()
	return &mockrepoclient.Clientset{RepoServerServiceClient: client}, &seen
}

func TestMultihubRepoCommitTimeReturnsCommitDate(t *testing.T) {
	app := pinTestApp()
	ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app, &defaultProj}}, nil)
	clientset, seen := revisionMetadataClient(t, &v1alpha1.RevisionMetadata{Date: &metav1.Time{Time: pinT2}}, nil)

	got, err := ctrl.multihubRepoCommitTime(clientset)(t.Context(), app, "abc123")
	require.NoError(t, err)
	assert.True(t, got.Equal(pinT2), "got %v", got)
	require.Len(t, *seen, 1)
	assert.Equal(t, "abc123", (*seen)[0].Revision)
	require.NotNil(t, (*seen)[0].Repo)
	assert.Equal(t, app.Spec.GetSource().RepoURL, (*seen)[0].Repo.Repo)
}

func TestMultihubRepoCommitTimeErrors(t *testing.T) {
	app := pinTestApp()
	ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app, &defaultProj}}, nil)

	failing, _ := revisionMetadataClient(t, nil, errors.New("repo-server down"))
	_, err := ctrl.multihubRepoCommitTime(failing)(t.Context(), app, "abc123")
	require.Error(t, err)

	undated, _ := revisionMetadataClient(t, &v1alpha1.RevisionMetadata{}, nil)
	_, err = ctrl.multihubRepoCommitTime(undated)(t.Context(), app, "abc123")
	require.Error(t, err, "a revision without a commit date can't be ordered")
}

func TestMultihubPinRevisionSkipsHelmAndOCISources(t *testing.T) {
	for name, mutate := range map[string]func(*v1alpha1.ApplicationSource){
		"helm": func(s *v1alpha1.ApplicationSource) {
			s.Chart = "my-chart"
			s.RepoURL = "https://charts.example.invalid"
		},
		"oci": func(s *v1alpha1.ApplicationSource) { s.RepoURL = "oci://registry.example.invalid/app" },
	} {
		t.Run(name, func(t *testing.T) {
			app := pinTestApp()
			mutate(app.Spec.Source)
			f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
			f.lookup["1.2.3"] = pinT1

			got, recompare := f.ctrl.multihubPinRevision(t.Context(), app, "1.2.3")
			assert.Equal(t, "1.2.3", got)
			assert.False(t, recompare)
			assert.Equal(t, 0, f.calls, "only git revisions are ordered by commit time")
			_, ok := f.pin(t)
			assert.False(t, ok)
		})
	}
}

func TestMultihubInitWiresCommitTimeLookupOnlyWhenEnabled(t *testing.T) {
	app := pinTestApp()
	ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app, &defaultProj}}, nil)
	clientset, _ := revisionMetadataClient(t, &v1alpha1.RevisionMetadata{Date: &metav1.Time{Time: pinT1}}, nil)

	ctrl.multihubCommitTime = nil
	ctrl.multihubInit(clientset)
	assert.Nil(t, ctrl.multihubCommitTime, "disabled: no lookup")

	t.Setenv(multihub.EnvHubID, "hub-x")
	ctrl.multihubInit(clientset)
	require.NotNil(t, ctrl.multihubCommitTime, "enabled: lookup wired to the repo-server")
	got, err := ctrl.multihubCommitTime(t.Context(), app, "abc123")
	require.NoError(t, err)
	assert.True(t, got.Equal(pinT1))
}

func TestMultihubInitLogsEverySetting(t *testing.T) {
	ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{pinTestApp(), &defaultProj}}, nil)
	clientset, _ := revisionMetadataClient(t, &v1alpha1.RevisionMetadata{Date: &metav1.Time{Time: pinT1}}, nil)
	t.Setenv(multihub.EnvHubID, "hub-x")
	t.Setenv(multihub.EnvLogDenialsResetAfter, "45s")
	hook := captureLogs(t)

	ctrl.multihubInit(clientset)

	var startup *log.Entry
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "multihub coordination enabled") {
			startup = e
		}
	}
	require.NotNil(t, startup, "startup line logged")
	assert.Equal(t, "hub-x", startup.Data["multihub.hub"])
	assert.Equal(t, "changes", startup.Data["multihub.log_denials"])
	assert.Equal(t, "45s", startup.Data["multihub.log_denials_reset_after"])
}

// captureLogs swaps the standard logger's hooks for a test hook, and lowers
// its level to debug so debug entries reach the hook (logrus only fires
// hooks for enabled levels), for the duration of the test.
func captureLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	hook := &logtest.Hook{}
	oldHooks := log.StandardLogger().ReplaceHooks(log.LevelHooks{})
	oldLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	log.AddHook(hook)
	t.Cleanup(func() {
		log.StandardLogger().ReplaceHooks(oldHooks)
		log.SetLevel(oldLevel)
	})
	return hook
}

func pinEntries(hook *logtest.Hook, event string) []*log.Entry {
	var out []*log.Entry
	for _, e := range hook.AllEntries() {
		if e.Data["multihub.path"] == "pin" && e.Data["multihub.event"] == event {
			out = append(out, e)
		}
	}
	return out
}

func TestMultihubPinRevisionReportsAdoption(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	f.pinFromHubA(t, "new222", pinT2)
	hook := captureLogs(t)
	adopted := multihub.DecisionsTotal.WithLabelValues("hub-b", "pin", "adopted")
	before := testutil.ToFloat64(adopted)

	f.ctrl.multihubPinRevision(t.Context(), app, "old111")

	assert.InDelta(t, before+1, testutil.ToFloat64(adopted), 0, "metric counts the adoption")
	entries := pinEntries(hook, "adopted")
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, log.InfoLevel, e.Level)
	assert.Equal(t, "hub-b", e.Data["multihub.hub"])
	assert.Equal(t, "old111", e.Data["multihub.candidate_revision"])
	assert.Equal(t, "new222", e.Data["multihub.pinned_revision"])
	assert.Equal(t, "hub-a", e.Data["multihub.pinned_by"])
	assert.Equal(t, app.Name, e.Data["application"])
}

func TestMultihubPinRevisionReportsAdvanceAndKeep(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	f.pinFromHubA(t, "old111", pinT1)
	hook := captureLogs(t)

	f.ctrl.multihubPinRevision(t.Context(), app, "new222")
	f.ctrl.multihubPinRevision(t.Context(), app, "new222")

	adv := pinEntries(hook, "advanced")
	require.Len(t, adv, 1)
	assert.Equal(t, log.InfoLevel, adv[0].Level)
	assert.Equal(t, "new222", adv[0].Data["multihub.pinned_revision"])
	kept := pinEntries(hook, "kept")
	require.Len(t, kept, 1)
	assert.Equal(t, log.DebugLevel, kept[0].Level)
}

func TestMultihubPinRevisionReportsLookupErrorOnce(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	hook := captureLogs(t)
	errs := multihub.DecisionsTotal.WithLabelValues("hub-b", "pin", "error")
	before := testutil.ToFloat64(errs)

	f.ctrl.multihubPinRevision(t.Context(), app, "unknown999")
	f.ctrl.multihubPinRevision(t.Context(), app, "unknown999")

	assert.InDelta(t, before+2, testutil.ToFloat64(errs), 0, "every failure is counted")
	entries := pinEntries(hook, "error")
	require.Len(t, entries, 2)
	assert.Equal(t, log.WarnLevel, entries[0].Level, "first failure warns")
	assert.Equal(t, log.DebugLevel, entries[1].Level, "a repeat of the same failure is collapsed")
	assert.Contains(t, entries[0].Data["multihub.reason"], "unknown999")
}

// operationFixture: hub-b's controller with a sync operation requested on
// app, sharing a fake spoke with a hub-a coordinator.
func operationFixture(t *testing.T) (*multihubPinFixture, *v1alpha1.Application) {
	t.Helper()
	app := pinTestApp()
	app.Operation = &v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{Revision: "abc123"}}
	f := newMultihubPinFixture(t, &fakeData{
		apps:              []runtime.Object{app, &defaultProj},
		manifestResponses: []*apiclient.ManifestResponse{{Manifests: []string{}}},
	}, app)
	return f, app
}

func (f *multihubPinFixture) mirror(t *testing.T) (multihub.OperationMirror, *v1alpha1.OperationState) {
	t.Helper()
	server, key := f.target(t)
	m, ok, err := f.hubA.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	require.True(t, ok, "operation mirrored")
	var state v1alpha1.OperationState
	require.NoError(t, json.Unmarshal(m.State, &state))
	return m, &state
}

func TestMultihubOperationMirrorsFinalStateBeforeReleasing(t *testing.T) {
	f, app := operationFixture(t)

	f.ctrl.processRequestedAppOperation(app)

	patched, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, synccommon.OperationSucceeded, patched.Status.OperationState.Phase)

	m, state := f.mirror(t)
	assert.Equal(t, "hub-b", m.Hub)
	assert.False(t, m.Running, "completed")
	assert.Equal(t, synccommon.OperationSucceeded, state.Phase)
	assert.True(t, patched.Status.OperationState.StartedAt.Equal(&state.StartedAt), "startedAt mirrored: it names the hooks")
	assert.Equal(t, "abc123", state.Operation.Sync.Revision)

	server, key := f.target(t)
	d, err := f.hubA.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, multihub.EventAcquired, d.Event, "lease released after the final mirror")
}

func TestMultihubMirrorRecordsARunningOperation(t *testing.T) {
	f, app := operationFixture(t)
	server, key := f.target(t)
	d, err := f.ctrl.multihub.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	state := &v1alpha1.OperationState{
		Operation: *app.Operation,
		Phase:     synccommon.OperationRunning,
		Message:   "waiting for completion of hook batch/Job/presync",
		// Local: metav1.Time decodes from JSON in local time, to the second.
		StartedAt: metav1.NewTime(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC).Local()),
		SyncResult: &v1alpha1.SyncOperationResult{
			Revision:  "abc123",
			Resources: v1alpha1.ResourceResults{{Kind: "Job", Name: "presync-abc123-presync-1795089600", HookType: "PreSync", HookPhase: synccommon.OperationRunning}},
		},
	}

	f.ctrl.multihubMirror(t.Context(), app, d.Generation, state)

	m, got := f.mirror(t)
	assert.True(t, m.Running)
	assert.Equal(t, d.Generation, m.Generation)
	assert.Equal(t, state, got)
}

func TestMultihubMirrorFailureWarns(t *testing.T) {
	f, app := operationFixture(t)
	hook := captureLogs(t)
	// hub-b doesn't hold the lease: fenced.
	f.ctrl.multihubMirror(t.Context(), app, 7, &v1alpha1.OperationState{Phase: synccommon.OperationRunning})

	var warned bool
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && e.Data["multihub.event"] == "mirror_error" {
			warned = true
			assert.Contains(t, e.Data["multihub.reason"], "not the operation lease holder")
		}
	}
	assert.True(t, warned, "a failed mirror is a warning: takeover would restart this operation")
}

// orphanFixture: hub-a was running a sync of app (PreSync hook in flight),
// mirrored it, and then died: its lease has expired. hub-b's controller has
// no operation on its own copy of app.
func orphanFixture(t *testing.T, app *v1alpha1.Application, data *fakeData) (*multihubPinFixture, *v1alpha1.OperationState) {
	t.Helper()
	f := newMultihubPinFixture(t, data, app)
	server, key := f.target(t)
	d, err := f.hubA.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	state := &v1alpha1.OperationState{
		Operation: v1alpha1.Operation{
			Sync:        &v1alpha1.SyncOperation{Revision: "abc123"},
			InitiatedBy: v1alpha1.OperationInitiator{Username: "alice"},
		},
		Phase:     synccommon.OperationRunning,
		Message:   "waiting for completion of hook batch/Job/presync",
		StartedAt: metav1.NewTime(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC).Local()),
		SyncResult: &v1alpha1.SyncOperationResult{
			Revision:  "abc123",
			Resources: v1alpha1.ResourceResults{{Kind: "Job", Name: "presync-abc123-presync-1795089600", HookType: "PreSync", HookPhase: synccommon.OperationRunning}},
		},
	}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, f.hubA.MirrorOperation(t.Context(), server, key, d.Generation, true, raw))
	f.expireLease(t)
	return f, state
}

// expireLease backdates the lease's renewTime past its duration, as if its
// holder had stopped renewing it.
func (f *multihubPinFixture) expireLease(t *testing.T) {
	t.Helper()
	_, key := f.target(t)
	leases := f.spoke.CoordinationV1().Leases(multihub.DefaultNamespace)
	lease, err := leases.Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	past := metav1.NewMicroTime(time.Now().Add(-2 * time.Minute))
	lease.Spec.RenewTime = &past
	_, err = leases.Update(t.Context(), lease, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func TestMultihubClaimOrphanResumesTheOperation(t *testing.T) {
	app := pinTestApp()
	f, mirrored := orphanFixture(t, app, &fakeData{apps: []runtime.Object{app, &defaultProj}})
	hook := captureLogs(t)

	require.True(t, f.ctrl.multihubClaimOrphan(t.Context(), app))

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Operation, "the operation is set on this hub's Application")
	assert.Equal(t, "abc123", got.Operation.Sync.Revision)
	assert.Equal(t, "alice", got.Operation.InitiatedBy.Username, "still alice's operation")
	require.NotNil(t, got.Status.OperationState)
	assert.True(t, mirrored.StartedAt.Equal(&got.Status.OperationState.StartedAt), "same startedAt: same hook names")
	assert.Equal(t, synccommon.OperationRunning, got.Status.OperationState.Phase)
	assert.Equal(t, mirrored.SyncResult.Resources, got.Status.OperationState.SyncResult.Resources, "progress carried over")
	assert.Contains(t, got.Operation.Info, &v1alpha1.Info{Name: "multihub", Value: "taken over from hub-a"})
	assert.NotNil(t, app.Operation, "the in-memory app too, so this refresh doesn't auto-sync")
	assert.Equal(t, 1, f.ctrl.appOperationQueue.Len(), "queued for the operation processor")

	server, key := f.target(t)
	d, err := f.ctrl.multihub.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, multihub.EventRenewed, d.Event, "hub-b holds the lease")

	var warned bool
	for _, e := range hook.AllEntries() {
		if e.Data["multihub.path"] == multihub.PathTakeover && e.Data["multihub.event"] == string(multihub.EventTakeover) {
			warned = e.Level == log.WarnLevel
			assert.Equal(t, "hub-a", e.Data["multihub.holder"], "who it was taken from")
		}
	}
	assert.True(t, warned, "a takeover is a warning")
}

func TestMultihubClaimOrphanLeavesLiveAndOwnOperationsAlone(t *testing.T) {
	app := pinTestApp()
	f, _ := orphanFixture(t, app, &fakeData{apps: []runtime.Object{app, &defaultProj}})
	server, key := f.target(t)

	// hub-b already has an operation of its own: never replaced.
	own := app.DeepCopy()
	own.Operation = &v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{Revision: "mine"}}
	assert.False(t, f.ctrl.multihubClaimOrphan(t.Context(), own))
	assert.Equal(t, "mine", own.Operation.Sync.Revision)

	// hub-a is alive again (renews): nothing to claim yet.
	_, err := f.hubA.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, f.ctrl.multihubClaimOrphan(t.Context(), app))
	assert.Nil(t, app.Operation)
	assert.Equal(t, 0, f.ctrl.appOperationQueue.Len())
}

func TestMultihubClaimOrphanDisabled(t *testing.T) {
	app := pinTestApp()
	ctrl := newFakeController(t.Context(), &fakeData{apps: []runtime.Object{app, &defaultProj}}, nil)
	assert.False(t, ctrl.multihubClaimOrphan(t.Context(), app))
}

// End to end through the refresh loop: hub-b's copy of an auto-sync app is
// OutOfSync, and hub-a died mid-sync. The refresh resumes hub-a's operation
// instead of auto-syncing a new one (which would run every hook again).
func TestMultihubRefreshResumesOrphanInsteadOfAutoSyncing(t *testing.T) {
	app := newFakeApp() // automated sync policy
	data := &fakeData{
		apps: []runtime.Object{app, &defaultProj},
		manifestResponses: []*apiclient.ManifestResponse{{
			Manifests: []string{orphanTestConfigMap}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "abc123",
		}},
		managedLiveObjs: map[kube.ResourceKey]*unstructured.Unstructured{},
	}
	f, mirrored := orphanFixture(t, app, data)
	f.lookup["abc123"] = pinT1

	key, err := cache.MetaNamespaceKeyFunc(app)
	require.NoError(t, err)
	f.ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatest.Pointer(), nil)
	f.ctrl.appRefreshQueue.Add(key)
	f.ctrl.processAppRefreshQueueItem()

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Operation)
	assert.False(t, got.Operation.InitiatedBy.Automated, "hub-a's (alice's) operation, not a new auto-sync")
	require.NotNil(t, got.Status.OperationState)
	assert.True(t, mirrored.StartedAt.Equal(&got.Status.OperationState.StartedAt))
}

// orphanTestConfigMap is missing from the (empty) live state, so the app is
// OutOfSync and, with automated sync, would auto-sync.
const orphanTestConfigMap = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"lab","namespace":"` + test.FakeDestNamespace + `"},"data":{"generation":"1"}}`

// returningFixture: this controller (hub-b) was running app's operation,
// stopped (partitioned, or its controller died), and hub-a took the
// operation over. app still shows hub-b's own last state.
func returningFixture(t *testing.T) (*multihubPinFixture, *v1alpha1.Application, *v1alpha1.OperationState, int32) {
	t.Helper()
	app := pinTestApp()
	started := metav1.NewTime(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC).Local())
	app.Operation = &v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{Revision: "abc123"}}
	app.Status.OperationState = &v1alpha1.OperationState{
		Operation: *app.Operation,
		Phase:     synccommon.OperationRunning,
		Message:   "waiting for completion of hook batch/Job/presync",
		StartedAt: started,
	}
	f := newMultihubPinFixture(t, &fakeData{
		apps:              []runtime.Object{app, &defaultProj},
		manifestResponses: []*apiclient.ManifestResponse{{Manifests: []string{}}},
	}, app)
	server, key := f.target(t)
	d, err := f.ctrl.multihub.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	f.ctrl.multihubMirror(t.Context(), app, d.Generation, app.Status.OperationState)
	f.expireLease(t)
	claim, err := f.hubA.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	require.Equal(t, multihub.ClaimClaimed, claim.Event)

	theirs := app.Status.OperationState.DeepCopy()
	theirs.Message = "hook presync running (on hub-a)"
	return f, app, theirs, claim.Generation
}

func (f *multihubPinFixture) hubAMirrors(t *testing.T, gen int32, state *v1alpha1.OperationState) {
	t.Helper()
	server, key := f.target(t)
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, f.hubA.MirrorOperation(t.Context(), server, key, gen, !state.Phase.Completed(), raw))
}

func TestMultihubReturningHubFollowsARunningTakeover(t *testing.T) {
	f, app, theirs, gen := returningFixture(t)
	f.hubAMirrors(t, gen, theirs)
	hook := captureLogs(t)

	f.ctrl.processRequestedAppOperation(app)

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Status.OperationState)
	assert.Equal(t, synccommon.OperationRunning, got.Status.OperationState.Phase, "not synced here, not failed either")
	assert.Equal(t, "hook presync running (on hub-a)", got.Status.OperationState.Message, "hub-a's progress shown")
	assert.NotNil(t, got.Operation, "still in progress")

	m, _ := f.mirror(t)
	assert.Equal(t, "hub-a", m.Hub, "hub-b must not take the operation back")

	var followed bool
	for _, e := range hook.AllEntries() {
		if e.Data["multihub.event"] == string(multihub.EventFollowing) {
			followed = true
			assert.Equal(t, "hub-a", e.Data["multihub.holder"])
		}
	}
	assert.True(t, followed, "logged")
}

// The dangerous case: hub-a finished and released before hub-b came back. A
// free lease must not let hub-b resume (and so repeat) the operation.
func TestMultihubReturningHubTakesTheFinalResult(t *testing.T) {
	f, app, theirs, gen := returningFixture(t)
	theirs.Phase = synccommon.OperationSucceeded
	theirs.Message = "successfully synced (all tasks run)"
	finished := metav1.NewTime(theirs.StartedAt.Add(time.Minute))
	theirs.FinishedAt = &finished
	f.hubAMirrors(t, gen, theirs)
	server, key := f.target(t)
	require.NoError(t, f.hubA.Release(t.Context(), server, key))

	f.ctrl.processRequestedAppOperation(app)

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Status.OperationState)
	assert.Equal(t, synccommon.OperationSucceeded, got.Status.OperationState.Phase)
	assert.Equal(t, "successfully synced (all tasks run)", got.Status.OperationState.Message, "hub-a's result, not a re-run")
	assert.Nil(t, got.Operation, "completed")

	m, _ := f.mirror(t)
	assert.Equal(t, "hub-a", m.Hub, "hub-b didn't re-run and re-mirror it")
	d, err := f.hubA.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, multihub.EventAcquired, d.Event, "hub-b didn't take the lease")

	ok, _ := f.ctrl.isRefreshRequested(f.ctrl.toAppKey(app.QualifiedName()))
	assert.True(t, ok, "refresh so the app's sync status catches up")
}

// A different operation on the mirror (another startedAt) is not ours: the
// C2 behavior stands (stop with an error while another hub holds the lease).
func TestMultihubReturningHubIgnoresADifferentOperation(t *testing.T) {
	f, app, theirs, gen := returningFixture(t)
	theirs.StartedAt = metav1.NewTime(theirs.StartedAt.Add(time.Hour))
	f.hubAMirrors(t, gen, theirs)

	f.ctrl.processRequestedAppOperation(app)

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, synccommon.OperationError, got.Status.OperationState.Phase)
}

// Found live in E8 (research/2026-11-20-c3-takeover.md): hub-b's Application
// still had its own previous, finished operation. Merging the mirrored state
// over it kept that operation's finishedAt, which on a Running operation
// means "retry pending": hub-b retried from scratch, dropped the mirrored
// results, and deleted and re-created the running hook.
func TestMultihubClaimOrphanReplacesThePreviousOperationState(t *testing.T) {
	app := pinTestApp()
	finished := metav1.NewTime(time.Date(2026, 11, 20, 11, 0, 30, 0, time.UTC))
	app.Status.OperationState = &v1alpha1.OperationState{
		Operation:  v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{Revision: "old111"}, Retry: v1alpha1.RetryStrategy{Limit: 5}},
		Phase:      synccommon.OperationSucceeded,
		Message:    "successfully synced (all tasks run)",
		StartedAt:  metav1.NewTime(time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)),
		FinishedAt: &finished,
		RetryCount: 2,
		SyncResult: &v1alpha1.SyncOperationResult{Revision: "old111", Revisions: []string{"old111"}},
	}
	f, mirrored := orphanFixture(t, app, &fakeData{apps: []runtime.Object{app, &defaultProj}})

	require.True(t, f.ctrl.multihubClaimOrphan(t.Context(), app))

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	want := mirrored.DeepCopy()
	want.Operation.Info = got.Status.OperationState.Operation.Info // the takeover note, checked elsewhere
	assert.Equal(t, want, got.Status.OperationState, "exactly the mirrored state: nothing left over from the previous operation")
	assert.Equal(t, mirrored.Operation.Retry, got.Operation.Retry)
}

// C3 gap 1 (research/2026-11-20-c3-takeover.md): a hub that sees another
// hub's running operation schedules a refresh for when that hub's lease
// expires, so a takeover doesn't wait for the next periodic refresh.
func TestMultihubClaimOrphanRequeuesAtLeaseExpiry(t *testing.T) {
	app := pinTestApp()
	f, _ := orphanFixture(t, app, &fakeData{apps: []runtime.Object{app, &defaultProj}})
	server, key := f.target(t)
	_, err := f.hubA.AcquireForOperation(t.Context(), server, key) // hub-a is alive: renews
	require.NoError(t, err)
	var got []time.Duration
	f.ctrl.multihubRequeueRefresh = func(a *v1alpha1.Application, after time.Duration) {
		assert.Equal(t, app.QualifiedName(), a.QualifiedName())
		got = append(got, after)
	}

	assert.False(t, f.ctrl.multihubClaimOrphan(t.Context(), app))

	require.Len(t, got, 1, "one refresh scheduled")
	assert.Greater(t, got[0], 55*time.Second, "at the lease's expiry (60s lease, just renewed)")
	assert.LessOrEqual(t, got[0], time.Minute+multihubClaimSlack)
}

func TestMultihubClaimOrphanDoesNotRequeueWithoutAnOrphan(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	called := false
	f.ctrl.multihubRequeueRefresh = func(*v1alpha1.Application, time.Duration) { called = true }
	assert.False(t, f.ctrl.multihubClaimOrphan(t.Context(), app))
	assert.False(t, called)
}

// C3 gap 3, controller side: the lease was claimed but this hub's
// Application couldn't be written. The claim is given back.
func TestMultihubClaimOrphanGivesTheClaimBackIfTheAppWriteFails(t *testing.T) {
	app := pinTestApp()
	f, _ := orphanFixture(t, app, &fakeData{apps: []runtime.Object{app, &defaultProj}})
	fakeAppCs := f.ctrl.applicationClientset.(*appclientset.Clientset)
	fakeAppCs.PrependReactor("patch", "applications", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcd unavailable")
	})

	assert.False(t, f.ctrl.multihubClaimOrphan(t.Context(), app))
	assert.Nil(t, app.Operation)

	m, _ := f.mirror(t)
	assert.Equal(t, "hub-a", m.Hub, "mirror handed back to hub-a's record")
	assert.True(t, m.Running)
	server, key := f.target(t)
	c, err := f.ctrl.multihub.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, multihub.ClaimClaimed, c.Event, "this hub can claim it on its next refresh")
}

// C3 gap 4: the returning hub shows when the operation actually finished
// (on the other hub), not when it found out.
func TestMultihubReturningHubKeepsTheOtherHubsFinishedAt(t *testing.T) {
	f, app, theirs, gen := returningFixture(t)
	theirs.Phase = synccommon.OperationSucceeded
	theirs.Message = "successfully synced (all tasks run)"
	finished := metav1.NewTime(time.Date(2026, 11, 20, 12, 2, 0, 0, time.UTC).Local())
	theirs.FinishedAt = &finished
	f.hubAMirrors(t, gen, theirs)

	f.ctrl.processRequestedAppOperation(app)

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Status.OperationState.FinishedAt)
	assert.True(t, finished.Equal(got.Status.OperationState.FinishedAt), "got %v", got.Status.OperationState.FinishedAt)
	assert.Nil(t, got.Operation)
}

func (f *multihubPinFixture) override(t *testing.T) (multihub.Override, bool) {
	t.Helper()
	server, key := f.target(t)
	o, ok, err := f.hubA.CurrentOverride(t.Context(), server, key)
	require.NoError(t, err)
	return o, ok
}

func (f *multihubPinFixture) hubAOverride(t *testing.T, rev string) {
	t.Helper()
	server, key := f.target(t)
	_, err := f.hubA.SetOverride(t.Context(), server, key, rev, "rollback by carol")
	require.NoError(t, err)
}

func multihubEntries(hook *logtest.Hook, event string) []*log.Entry {
	var out []*log.Entry
	for _, e := range hook.AllEntries() {
		if e.Data["multihub.event"] == event {
			out = append(out, e)
		}
	}
	return out
}

// C5: a rollback (a user's sync to a specific revision) on this hub records
// an override every hub sees, before syncing.
func TestMultihubRollbackRecordsTheOverride(t *testing.T) {
	f, app := operationFixture(t)
	app.Operation.InitiatedBy = v1alpha1.OperationInitiator{Username: "alice"}
	hook := captureLogs(t)

	f.ctrl.processRequestedAppOperation(app)

	o, ok := f.override(t)
	require.True(t, ok, "override recorded")
	assert.Equal(t, "abc123", o.Revision)
	assert.Equal(t, "hub-b", o.By)
	assert.Contains(t, o.Reason, "alice")
	entries := multihubEntries(hook, "override_set")
	require.Len(t, entries, 1)
	assert.Equal(t, log.InfoLevel, entries[0].Level)
	assert.Equal(t, "abc123", entries[0].Data["multihub.override_revision"])
}

func TestMultihubSyncToTargetRevisionClearsTheOverride(t *testing.T) {
	for _, rev := range []string{"", "HEAD"} {
		t.Run("revision="+rev, func(t *testing.T) {
			f, app := operationFixture(t)
			app.Operation.Sync.Revision = rev
			app.Operation.InitiatedBy = v1alpha1.OperationInitiator{Username: "bob"}
			f.hubAOverride(t, "e264214")
			hook := captureLogs(t)

			f.ctrl.processRequestedAppOperation(app)

			_, ok := f.override(t)
			assert.False(t, ok, "cleared")
			entries := multihubEntries(hook, "override_cleared")
			require.Len(t, entries, 1)
			assert.Equal(t, "e264214", entries[0].Data["multihub.override_revision"])
		})
	}
}

func TestMultihubAutomatedAndPartialSyncsLeaveTheOverride(t *testing.T) {
	cases := map[string]func(*v1alpha1.Operation){
		"automated to a revision": func(op *v1alpha1.Operation) {
			op.InitiatedBy = v1alpha1.OperationInitiator{Automated: true}
		},
		"user, partial, to HEAD": func(op *v1alpha1.Operation) {
			op.InitiatedBy = v1alpha1.OperationInitiator{Username: "bob"}
			op.Sync.Revision = ""
			op.Sync.Resources = []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Name: "lab"}}
		},
		"user, dry run, to HEAD": func(op *v1alpha1.Operation) {
			op.InitiatedBy = v1alpha1.OperationInitiator{Username: "bob"}
			op.Sync.Revision = ""
			op.Sync.DryRun = true
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, app := operationFixture(t)
			mutate(app.Operation)
			f.hubAOverride(t, "e264214")

			f.ctrl.processRequestedAppOperation(app)

			o, ok := f.override(t)
			require.True(t, ok)
			assert.Equal(t, "e264214", o.Revision, "untouched")
			assert.Equal(t, "hub-a", o.By)
		})
	}
}

// If the override can't be recorded, the rollback would be silently reverted
// by the other hubs (E5): fail it instead.
func TestMultihubRollbackFailsIfTheOverrideCantBeRecorded(t *testing.T) {
	f, app := operationFixture(t)
	app.Operation.InitiatedBy = v1alpha1.OperationInitiator{Username: "alice"}
	f.spoke.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	f.ctrl.processRequestedAppOperation(app)

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Status.OperationState)
	assert.Equal(t, synccommon.OperationError, got.Status.OperationState.Phase)
	assert.Contains(t, got.Status.OperationState.Message, "override")
}

// C5, E5 unit-sized: hub-b rolled back (override recorded). This hub's
// auto-sync, with the app OutOfSync against HEAD, must not revert it.
func TestMultihubAutoSyncStandsDownWhileAnOverrideExists(t *testing.T) {
	for _, overridden := range []bool{true, false} {
		t.Run(fmt.Sprintf("override=%v", overridden), func(t *testing.T) {
			app := newFakeApp() // automated sync policy
			data := &fakeData{
				apps: []runtime.Object{app, &defaultProj},
				manifestResponses: []*apiclient.ManifestResponse{{
					Manifests: []string{orphanTestConfigMap}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "abc123",
				}},
				managedLiveObjs: map[kube.ResourceKey]*unstructured.Unstructured{},
			}
			f := newMultihubPinFixture(t, data, app)
			f.lookup["abc123"] = pinT1
			if overridden {
				f.hubAOverride(t, "e264214")
			}
			hook := captureLogs(t)

			key, err := cache.MetaNamespaceKeyFunc(app)
			require.NoError(t, err)
			f.ctrl.requestAppRefresh(app.QualifiedName(), CompareWithLatest.Pointer(), nil)
			f.ctrl.appRefreshQueue.Add(key)
			f.ctrl.processAppRefreshQueueItem()

			got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, got.Status.Sync.Status, "still compared against HEAD: honest status")
			if !overridden {
				assert.NotNil(t, got.Operation, "control: auto-sync runs")
				return
			}
			assert.Nil(t, got.Operation, "auto-sync stood down")
			entries := multihubEntries(hook, string(multihub.EventOverridden))
			require.Len(t, entries, 1)
			assert.Equal(t, log.InfoLevel, entries[0].Level)
			assert.Equal(t, "e264214", entries[0].Data["multihub.override_revision"])
		})
	}
}

func appEvents(ctrl *ApplicationController, reason string) []*corev1.Event {
	var out []*corev1.Event
	for _, action := range ctrl.kubeClientset.(*k8sfake.Clientset).Actions() {
		create, ok := action.(k8stesting.CreateAction)
		if !ok || create.GetResource().Resource != "events" {
			continue
		}
		if e, ok := create.GetObject().(*corev1.Event); ok && e.Reason == reason {
			out = append(out, e)
		}
	}
	return out
}

func TestMultihubPausedAutoSyncRaisesOneEvent(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	f.hubAOverride(t, "e264214")

	assert.True(t, f.ctrl.multihubOverridden(t.Context(), app))
	assert.True(t, f.ctrl.multihubOverridden(t.Context(), app))

	events := appEvents(f.ctrl, EventReasonMultihubAutoSyncPaused)
	require.Len(t, events, 1, "once per change, not every refresh")
	assert.Equal(t, corev1.EventTypeNormal, events[0].Type)
	assert.Contains(t, events[0].Message, "e264214")
}

func TestMultihubFingerprint(t *testing.T) {
	base := newFakeApp()
	fp := func(mutate func(*v1alpha1.Application), server string) string {
		a := base.DeepCopy()
		mutate(a)
		return multihubFingerprint(a, server)
	}
	same := func(*v1alpha1.Application) {}
	ref := fp(same, "https://spoke")
	require.NotEmpty(t, ref)
	assert.Len(t, ref, 32)

	assert.Equal(t, ref, fp(same, "https://spoke"), "deterministic")
	assert.Equal(t, ref, fp(func(a *v1alpha1.Application) { a.Spec.SyncPolicy.Automated = nil }, "https://spoke"), "auto-sync may differ per hub")
	assert.Equal(t, ref, fp(func(a *v1alpha1.Application) {
		a.Labels = map[string]string{"x": "y"}
		a.Annotations = map[string]string{"p": "q"}
	}, "https://spoke"), "metadata may differ")
	assert.Equal(t, ref, fp(func(a *v1alpha1.Application) { a.Spec.Destination.Name = "prod-east" }, "https://spoke"), "destination compared as the resolved server")

	assert.NotEqual(t, ref, fp(same, "https://other"), "different cluster")
	assert.NotEqual(t, ref, fp(func(a *v1alpha1.Application) {
		a.Spec.Source.Helm = &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{{Name: "image.tag", Value: "1.5.0"}}}
	}, "https://spoke"), "a parameter override")
	assert.NotEqual(t, ref, fp(func(a *v1alpha1.Application) { a.Spec.Source.TargetRevision = "release" }, "https://spoke"))
	assert.NotEqual(t, ref, fp(func(a *v1alpha1.Application) { a.Spec.Destination.Namespace = "other" }, "https://spoke"))
	assert.NotEqual(t, ref, fp(func(a *v1alpha1.Application) { a.Spec.Project = "other" }, "https://spoke"))
	assert.NotEqual(t, ref, fp(func(a *v1alpha1.Application) {
		a.Spec.SyncPolicy.SyncOptions = v1alpha1.SyncOptions{"ServerSideApply=true"}
	}, "https://spoke"))
	assert.NotEqual(t, ref, fp(func(a *v1alpha1.Application) {
		a.Spec.IgnoreDifferences = []v1alpha1.ResourceIgnoreDifferences{{Kind: "ConfigMap", JSONPointers: []string{"/data"}}}
	}, "https://spoke"))
}

// shareRefreshFixture: hub-b's controller, the app pinned at "abc123", and a
// refresh that compares at it.
func shareRefreshFixture(t *testing.T, app *v1alpha1.Application, onGenerate func(*apiclient.ManifestRequest)) *multihubPinFixture {
	t.Helper()
	data := &fakeData{
		apps: []runtime.Object{app, &defaultProj},
		manifestResponses: []*apiclient.ManifestResponse{
			{Manifests: []string{orphanTestConfigMap}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "abc123"},
			{Manifests: []string{orphanTestConfigMap}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "abc123"},
		},
		managedLiveObjs:    map[kube.ResourceKey]*unstructured.Unstructured{},
		onGenerateManifest: onGenerate,
	}
	f := newMultihubPinFixture(t, data, app)
	f.lookup["abc123"] = pinT1
	f.pinFromHubA(t, "abc123", pinT1)
	return f
}

func (f *multihubPinFixture) refresh(t *testing.T, app *v1alpha1.Application, level CompareWith) {
	t.Helper()
	key, err := cache.MetaNamespaceKeyFunc(app)
	require.NoError(t, err)
	f.ctrl.requestAppRefresh(app.QualifiedName(), level.Pointer(), nil)
	f.ctrl.appRefreshQueue.Add(key)
	f.ctrl.processAppRefreshQueueItem()
}

func TestMultihubRefreshPublishesItsResult(t *testing.T) {
	app := pinTestApp()
	f := shareRefreshFixture(t, app, nil)

	f.refresh(t, app, CompareWithLatest)

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	server, key := f.target(t)
	lease, err := f.spoke.CoordinationV1().Leases(multihub.DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	rec, ok := multihub.RefreshRecordOf(lease)
	require.True(t, ok, "published")
	assert.Equal(t, "hub-b", rec.By)
	assert.Equal(t, "abc123", rec.Revision)
	assert.Equal(t, multihubFingerprint(got, server), rec.Fingerprint)

	raw, err := f.hubA.FetchResult(t.Context(), server, key, rec.ResultHash)
	require.NoError(t, err)
	var res multihubSharedResult
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.Equal(t, got.Status.Sync.Status, res.Sync.Status)
	assert.Equal(t, "abc123", res.Sync.Revision)
	assert.Equal(t, got.Status.Health.Status, res.Health.Status)
	assert.Equal(t, got.Status.Resources, res.Resources)
}

func TestMultihubRefreshDoesNotPublishForMultiSourceApps(t *testing.T) {
	app := pinTestApp()
	f := shareRefreshFixture(t, app, nil)
	multi := app.DeepCopy()
	multi.Spec.Sources = v1alpha1.ApplicationSources{*app.Spec.Source, *app.Spec.Source}
	multi.Spec.Source = nil

	f.ctrl.multihubPublishRefresh(t.Context(), multi, multihubSharedResult{})

	_, key := f.target(t)
	lease, err := f.spoke.CoordinationV1().Leases(multihub.DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	_, ok := multihub.RefreshRecordOf(lease)
	assert.False(t, ok, "not shareable: no pin to agree on")
}

// skipFixture: hub-b's copy of a single-source app, last reconciled 10 min
// ago (the test resync period is 1 min, so a periodic refresh is due), with
// the pin at abc123. publish makes hub-a publish a refresh; generated counts
// this hub's manifest generations, i.e. its own comparisons.
type skipFixture struct {
	*multihubPinFixture
	app       *v1alpha1.Application
	generated int
}

func newSkipFixture(t *testing.T) *skipFixture {
	t.Helper()
	app := pinTestApp()
	old := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	app.Status.ReconciledAt = &old
	app.Status.Sync = v1alpha1.SyncStatus{
		Status:     v1alpha1.SyncStatusCodeOutOfSync,
		Revision:   "old111",
		ComparedTo: v1alpha1.ComparedTo{Source: *app.Spec.Source, Destination: app.Spec.Destination},
	}
	sf := &skipFixture{app: app}
	sf.multihubPinFixture = shareRefreshFixture(t, app, func(*apiclient.ManifestRequest) { sf.generated++ })
	return sf
}

func (sf *skipFixture) peerResult() multihubSharedResult {
	return multihubSharedResult{
		Sync: v1alpha1.SyncStatus{
			Status:     v1alpha1.SyncStatusCodeSynced,
			Revision:   "abc123",
			ComparedTo: v1alpha1.ComparedTo{Source: *sf.app.Spec.Source, Destination: sf.app.Spec.Destination},
		},
		Health:    v1alpha1.AppHealthStatus{Status: "Healthy"},
		Resources: []v1alpha1.ResourceStatus{{Kind: "ConfigMap", Name: "lab", Namespace: test.FakeDestNamespace, Status: v1alpha1.SyncStatusCodeSynced, Version: "v1"}},
	}
}

func (sf *skipFixture) publish(t *testing.T, by *multihub.Coordinator, revision, fingerprint string, res multihubSharedResult) {
	t.Helper()
	server, key := sf.target(t)
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	_, err = by.PublishRefresh(t.Context(), server, key, revision, fingerprint, raw)
	require.NoError(t, err)
	// let hub-b's Lease watch see it
	require.Eventually(t, func() bool {
		v, ok := sf.ctrl.multihub.Cached(server, key)
		return ok && v.HasRefresh && v.Refresh.By == by.HubID() && v.Refresh.Revision == revision && v.Refresh.Fingerprint == fingerprint
	}, 10*time.Second, 20*time.Millisecond)
	t.Cleanup(sf.ctrl.multihub.StopWatches)
}

func (sf *skipFixture) fingerprint(t *testing.T) string {
	server, _ := sf.target(t)
	return multihubFingerprint(sf.app, server)
}

func (sf *skipFixture) periodicRefresh(t *testing.T) *v1alpha1.Application {
	t.Helper()
	key, err := cache.MetaNamespaceKeyFunc(sf.app)
	require.NoError(t, err)
	sf.ctrl.appRefreshQueue.Add(key)
	sf.ctrl.processAppRefreshQueueItem()
	got, err := sf.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(sf.app.Namespace).Get(t.Context(), sf.app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

// C7: hub-a refreshed this app moments ago; hub-b's periodic refresh comes
// due. hub-b shows hub-a's result and doesn't compare.
func TestMultihubPeriodicRefreshUsesTheOtherHubsResult(t *testing.T) {
	sf := newSkipFixture(t)
	sf.ctrl.multihub = multihubWithSpoke(sf, true)
	sf.publish(t, sf.hubA, "abc123", sf.fingerprint(t), sf.peerResult())
	before := time.Now()

	got := sf.periodicRefresh(t)

	assert.Equal(t, 0, sf.generated, "no comparison on hub-b")
	assert.Equal(t, v1alpha1.SyncStatusCodeSynced, got.Status.Sync.Status, "hub-a's result")
	assert.Equal(t, "abc123", got.Status.Sync.Revision)
	assert.Equal(t, "Healthy", string(got.Status.Health.Status))
	assert.Equal(t, sf.peerResult().Resources, got.Status.Resources)
	require.NotNil(t, got.Status.ReconciledAt)
	assert.False(t, got.Status.ReconciledAt.Time.Before(before.Truncate(time.Second)), "reconciledAt moves on, so hub-b's next periodic refresh is a full period away")
}

func TestMultihubPeriodicRefreshComparesWhenTheSharedResultDoesNotApply(t *testing.T) {
	cases := map[string]func(t *testing.T, sf *skipFixture){
		"nothing published": func(t *testing.T, sf *skipFixture) {
			server, key := sf.target(t)
			_, err := sf.hubA.AgreeRevision(t.Context(), server, key, multihub.Candidate{Revision: "abc123", CommitTime: pinT1})
			require.NoError(t, err)
			require.Eventually(t, func() bool { _, ok := sf.ctrl.multihub.Cached(server, key); return ok }, 10*time.Second, 20*time.Millisecond)
			t.Cleanup(sf.ctrl.multihub.StopWatches)
		},
		"another definition": func(t *testing.T, sf *skipFixture) {
			sf.publish(t, sf.hubA, "abc123", "some-other-fingerprint", sf.peerResult())
		},
		"another revision than the pin": func(t *testing.T, sf *skipFixture) {
			sf.publish(t, sf.hubA, "def456", sf.fingerprint(t), sf.peerResult())
		},
		"published by this hub": func(t *testing.T, sf *skipFixture) {
			sf.publish(t, sf.ctrl.multihub, "abc123", sf.fingerprint(t), sf.peerResult())
		},
		"too old": func(t *testing.T, sf *skipFixture) {
			sf.publish(t, sf.hubA, "abc123", sf.fingerprint(t), sf.peerResult())
			server, key := sf.target(t)
			leases := sf.spoke.CoordinationV1().Leases(multihub.DefaultNamespace)
			lease, err := leases.Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
			require.NoError(t, err)
			lease.Annotations[multihub.AnnotationRefreshedAt] = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
			_, err = leases.Update(t.Context(), lease, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				v, _ := sf.ctrl.multihub.Cached(server, key)
				return time.Since(v.Refresh.At) > time.Minute
			}, 10*time.Second, 20*time.Millisecond)
		},
		"a user asked for a refresh": func(t *testing.T, sf *skipFixture) {
			sf.publish(t, sf.hubA, "abc123", sf.fingerprint(t), sf.peerResult())
			sf.app.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: string(v1alpha1.RefreshTypeNormal)}
			require.NoError(t, sf.ctrl.appInformer.GetIndexer().Update(sf.app))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			sf := newSkipFixture(t)
			sf.ctrl.multihub = multihubWithSpoke(sf, true)
			setup(t, sf)
			sf.periodicRefresh(t)
			assert.Positive(t, sf.generated, "hub-b compares for itself")
		})
	}
}

func TestMultihubPeriodicRefreshComparesWhenSharingIsOff(t *testing.T) {
	sf := newSkipFixture(t)
	sf.ctrl.multihub = multihubWithSpoke(sf, false)
	sf.publish(t, sf.hubA, "abc123", sf.fingerprint(t), sf.peerResult())
	sf.periodicRefresh(t)
	assert.Positive(t, sf.generated)
}

// multihubWithSpoke: hub-b's coordinator on the fixture's spoke, with shared
// refresh on or off.
func multihubWithSpoke(sf *skipFixture, share bool) *multihub.Coordinator {
	clients := func(string) (kubernetes.Interface, error) { return sf.spoke, nil }
	return multihub.New(multihub.Config{HubID: "hub-b", LeaseDuration: time.Minute, ShareRefresh: share, Telemetry: multihub.DefaultTelemetry()}, clients)
}

// A controller refresh request that is pending when the periodic refresh
// comes due (e.g. a resource just changed on the spoke) is this hub's own
// work: don't skip it.
func TestMultihubPeriodicRefreshComparesWhenARefreshIsPending(t *testing.T) {
	sf := newSkipFixture(t)
	sf.ctrl.multihub = multihubWithSpoke(sf, true)
	sf.publish(t, sf.hubA, "abc123", sf.fingerprint(t), sf.peerResult())
	sf.ctrl.requestAppRefresh(sf.app.QualifiedName(), CompareWithRecent.Pointer(), nil)

	sf.periodicRefresh(t)

	assert.Positive(t, sf.generated, "the pending request is honored locally")
}

// A skipping hub still rebuilds its resource tree (for the UI) from its own
// live watch of the spoke.
func TestMultihubSkippedRefreshRebuildsTheResourceTree(t *testing.T) {
	sf := newSkipFixture(t)
	sf.ctrl.multihub = multihubWithSpoke(sf, true)
	sf.periodicRefresh(t) // a local refresh first: caches the managed resources
	require.Positive(t, sf.generated)
	sentinel := &v1alpha1.ApplicationTree{Nodes: []v1alpha1.ResourceNode{{ResourceRef: v1alpha1.ResourceRef{Kind: "Sentinel", Name: "stale"}}}}
	instance := sf.app.InstanceName(sf.ctrl.namespace)
	require.NoError(t, sf.ctrl.cache.SetAppResourcesTree(instance, sentinel))
	due, err := sf.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(sf.app.Namespace).Get(t.Context(), sf.app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	old := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	due.Status.ReconciledAt = &old
	require.NoError(t, sf.ctrl.appInformer.GetIndexer().Update(due))
	sf.app = due
	sf.publish(t, sf.hubA, "abc123", sf.fingerprint(t), sf.peerResult())
	generated := sf.generated

	sf.periodicRefresh(t)

	require.Equal(t, generated, sf.generated, "skipped")
	var tree v1alpha1.ApplicationTree
	require.NoError(t, sf.ctrl.cache.GetAppResourcesTree(instance, &tree))
	for _, n := range tree.Nodes {
		assert.NotEqual(t, "Sentinel", n.Kind, "tree rebuilt from the live watch")
	}
}

func periodController(t *testing.T, cm map[string]string, multihubOn bool) *ApplicationController {
	t.Helper()
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}, configMapData: cm}, app)
	if !multihubOn {
		f.ctrl.multihub = nil
	}
	return f.ctrl
}

func TestMultihubRefreshPeriodFollowsArgocdCM(t *testing.T) {
	// the fake controller starts with a 1m period
	assert.Equal(t, 10*time.Minute, periodController(t, map[string]string{"timeout.reconciliation": "10m"}, true).refreshPeriod())
	assert.Equal(t, 2*time.Minute, periodController(t, map[string]string{"timeout.reconciliation": "120s"}, true).refreshPeriod())
	assert.Equal(t, time.Minute, periodController(t, map[string]string{}, true).refreshPeriod(), "key missing: startup value")
	assert.Equal(t, time.Minute, periodController(t, map[string]string{"timeout.reconciliation": "soon"}, true).refreshPeriod(), "invalid: startup value")
	assert.Equal(t, time.Minute, periodController(t, map[string]string{"timeout.reconciliation": "10m"}, false).refreshPeriod(), "multihub off: upstream behavior")
}

func TestMultihubRefreshPeriodChangesLive(t *testing.T) {
	ctrl := periodController(t, map[string]string{"timeout.reconciliation": "10m"}, true)
	require.Equal(t, 10*time.Minute, ctrl.refreshPeriod())
	cms := ctrl.kubeClientset.CoreV1().ConfigMaps(test.FakeArgoCDNamespace)
	cm, err := cms.Get(t.Context(), "argocd-cm", metav1.GetOptions{})
	require.NoError(t, err)
	cm.Data["timeout.reconciliation"] = "2m"
	_, err = cms.Update(t.Context(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return ctrl.refreshPeriod() == 2*time.Minute }, 10*time.Second, 20*time.Millisecond, "no restart needed")
}

// The expiry check uses the live period: an app refreshed 5 minutes ago is
// not due under a 10m period, and is under a 2m period.
func TestMultihubPeriodicRefreshUsesTheLivePeriod(t *testing.T) {
	for period, wantRefresh := range map[string]bool{"10m": false, "2m": true} {
		t.Run(period, func(t *testing.T) {
			app := pinTestApp()
			fiveAgo := metav1.NewTime(time.Now().Add(-5 * time.Minute))
			app.Status.ReconciledAt = &fiveAgo
			app.Status.Sync = v1alpha1.SyncStatus{Status: v1alpha1.SyncStatusCodeSynced, Revision: "abc123",
				ComparedTo: v1alpha1.ComparedTo{Source: *app.Spec.Source, Destination: app.Spec.Destination}}
			generated := 0
			f := newMultihubPinFixture(t, &fakeData{
				apps:               []runtime.Object{app, &defaultProj},
				configMapData:      map[string]string{"timeout.reconciliation": period},
				manifestResponses:  []*apiclient.ManifestResponse{{Manifests: []string{}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "abc123"}},
				managedLiveObjs:    map[kube.ResourceKey]*unstructured.Unstructured{},
				onGenerateManifest: func(*apiclient.ManifestRequest) { generated++ },
			}, app)
			f.lookup["abc123"] = pinT1
			key, err := cache.MetaNamespaceKeyFunc(app)
			require.NoError(t, err)
			f.ctrl.appRefreshQueue.Add(key)
			f.ctrl.processAppRefreshQueueItem()
			assert.Equal(t, wantRefresh, generated > 0)
		})
	}
}

func TestMultihubResyncLoopFollowsTheLivePeriod(t *testing.T) {
	ctrl := periodController(t, map[string]string{"timeout.reconciliation": "2m"}, true)
	ctrl.statusRefreshJitter = 0
	cms := ctrl.kubeClientset.CoreV1().ConfigMaps(test.FakeArgoCDNamespace)
	var waits []time.Duration
	ctrl.multihubWait = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		switch len(waits) {
		case 1:
			return true
		case 2:
			assert.Equal(t, 1, ctrl.appRefreshQueue.Len(), "every app queued after the first wait")
			cm, err := cms.Get(t.Context(), "argocd-cm", metav1.GetOptions{})
			require.NoError(t, err)
			cm.Data["timeout.reconciliation"] = "90s"
			_, err = cms.Update(t.Context(), cm, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.Eventually(t, func() bool { return ctrl.refreshPeriod() == 90*time.Second }, 10*time.Second, 20*time.Millisecond)
			return true
		default:
			return false
		}
	}

	done := make(chan struct{})
	go func() { ctrl.multihubResyncLoop(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("loop didn't stop when the wait was cancelled")
	}
	require.Len(t, waits, 3)
	assert.Equal(t, 2*time.Minute, waits[0])
	assert.Equal(t, 2*time.Minute, waits[1])
	assert.Equal(t, 90*time.Second, waits[2], "the new period, without a restart")
}

func selfRegistration(lbls map[string]string) *corev1.Secret {
	l := map[string]string{"argocd.argoproj.io/secret-type": "cluster"}
	maps.Copy(l, lbls)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "in-cluster", Namespace: test.FakeArgoCDNamespace, Labels: l},
		Data:       map[string][]byte{"name": []byte("in-cluster"), "server": []byte("https://kubernetes.default.svc"), "config": []byte("{}")},
	}
}

func hubMembership(t *testing.T, members string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal([]byte(`
apiVersion: multihub.argoproj.io/v1alpha1
kind: HubMembership
metadata: {name: default, namespace: `+test.FakeArgoCDNamespace+`}
spec:
  members:
`+members), &obj.Object))
	return obj
}

// ownStateController: hub-b's controller with the given self-registration
// labels, watching a fake HubMembership (none if members is "").
func ownStateController(t *testing.T, lbls map[string]string, members string) (*ApplicationController, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}, additionalObjs: []runtime.Object{selfRegistration(lbls)}}, app)
	var objs []runtime.Object
	if members != "" {
		objs = append(objs, hubMembership(t, members))
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(apiruntime.NewScheme(),
		map[schema.GroupVersionResource]string{multihub.HubMembershipGVR: "HubMembershipList"}, objs...)
	f.ctrl.MultihubWatchMembership(dyn)
	t.Cleanup(func() { f.ctrl.multihubMembership.Stop() })
	return f.ctrl, dyn
}

func ownStateEventually(t *testing.T, ctrl *ApplicationController, want multihub.MemberState, wantMember, wantDeclared bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		st, member, declared := ctrl.multihubOwnState(t.Context())
		return declared == wantDeclared && member == wantMember && (!member || st == want)
	}, 10*time.Second, 20*time.Millisecond)
}

func TestMultihubOwnStateFromSelfRegistrationLabels(t *testing.T) {
	role := map[string]string{"multihub.argoproj.io/role": "hub", "multihub.argoproj.io/hub-id": "hub-b"}
	ctrl, _ := ownStateController(t, role, `
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: active
`)
	ownStateEventually(t, ctrl, multihub.StateActive, true, true)
}

func TestMultihubOwnStateIDOverridesSelector(t *testing.T) {
	ctrl, _ := ownStateController(t, map[string]string{"multihub.argoproj.io/role": "hub"}, `
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: active
    - id: hub-b
      state: watching
`)
	ownStateEventually(t, ctrl, multihub.StateWatching, true, true)
}

func TestMultihubOwnStateNotAMember(t *testing.T) {
	ctrl, _ := ownStateController(t, map[string]string{"region": "eu-west"}, `
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: active
`)
	ownStateEventually(t, ctrl, "", false, true)
}

func TestMultihubOwnStateWithoutAHubMembership(t *testing.T) {
	ctrl, _ := ownStateController(t, nil, "")
	time.Sleep(300 * time.Millisecond)
	st, member, declared := ctrl.multihubOwnState(t.Context())
	assert.False(t, declared, "no HubMembership: today's behavior")
	assert.True(t, member)
	assert.Equal(t, multihub.StateActive, st)
}

// Selectors that give this hub two different states: keep the state it had
// (status quo), don't guess.
func TestMultihubOwnStateKeepsTheStatusQuoOnAConflict(t *testing.T) {
	lbls := map[string]string{"multihub.argoproj.io/role": "hub", "region": "eu-west"}
	ctrl, dyn := ownStateController(t, lbls, `
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: active
`)
	ownStateEventually(t, ctrl, multihub.StateActive, true, true)

	conflicting := hubMembership(t, `
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: active
    - selector: {matchLabels: {region: eu-west}}
      state: watching
`)
	_, err := dyn.Resource(multihub.HubMembershipGVR).Namespace(test.FakeArgoCDNamespace).Update(t.Context(), conflicting, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		m, _ := ctrl.multihubMembership.Current()
		return len(m.Members) == 2
	}, 10*time.Second, 20*time.Millisecond)

	st, member, declared := ctrl.multihubOwnState(t.Context())
	assert.True(t, declared)
	assert.True(t, member)
	assert.Equal(t, multihub.StateActive, st, "kept")
}

func hubRecordOn(t *testing.T, f *multihubPinFixture, id string) (*coordinationv1.Lease, bool) {
	t.Helper()
	lease, err := f.spoke.CoordinationV1().Leases(multihub.DefaultNamespace).Get(t.Context(), multihub.HubRecordName(id), metav1.GetOptions{})
	if err != nil {
		return nil, false
	}
	return lease, true
}

func TestMultihubHeartbeatPublishesThisHubsRecord(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj},
		additionalObjs: []runtime.Object{selfRegistration(map[string]string{"multihub.argoproj.io/role": "hub", "multihub.argoproj.io/hub-id": "hub-b"})}}, app)

	f.ctrl.multihubHeartbeat(t.Context())

	lease, ok := hubRecordOn(t, f, "hub-b")
	require.True(t, ok, "published on the spoke")
	assert.Equal(t, "hub-b", lease.Annotations[multihub.AnnotationHubID])
	assert.Contains(t, lease.Annotations[multihub.AnnotationHubLabels], `"multihub.argoproj.io/role":"hub"`, "labels from self-registration")
	assert.NotContains(t, lease.Annotations[multihub.AnnotationHubLabels], "secret-type", "only this hub's own labels, not Argo's bookkeeping")
}

func TestMultihubHeartbeatLoopRepeats(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}}, app)
	var waits []time.Duration
	f.ctrl.multihubWait = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return len(waits) < 3
	}
	f.ctrl.multihubHeartbeatLoop(t.Context())
	_, ok := hubRecordOn(t, f, "hub-b")
	assert.True(t, ok, "published before the first wait")
	assert.Equal(t, []time.Duration{multihubHeartbeatInterval, multihubHeartbeatInterval, multihubHeartbeatInterval}, waits)
}

// withMembership makes hub-b's controller follow a HubMembership with the
// given members and waits until it's in force.
func withMembership(t *testing.T, f *multihubPinFixture, members string) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(apiruntime.NewScheme(),
		map[schema.GroupVersionResource]string{multihub.HubMembershipGVR: "HubMembershipList"}, hubMembership(t, members))
	f.ctrl.MultihubWatchMembership(dyn)
	t.Cleanup(func() { f.ctrl.multihubMembership.Stop() })
	require.Eventually(t, func() bool { _, ok := f.ctrl.multihubMembership.Current(); return ok }, 10*time.Second, 20*time.Millisecond)
}

const (
	hubBWatching = `
    - {id: hub-a, state: active}
    - {id: hub-b, state: watching}
`
	hubBNotAMember = `
    - {id: hub-a, state: active}
`
)

func TestMultihubWatchingOrNonMemberHubDoesNotAutoSync(t *testing.T) {
	for name, members := range map[string]string{"watching": hubBWatching, "not a member": hubBNotAMember} {
		t.Run(name, func(t *testing.T) {
			app := newFakeApp() // automated sync policy, OutOfSync below
			f := newMultihubPinFixture(t, &fakeData{
				apps: []runtime.Object{app, &defaultProj},
				manifestResponses: []*apiclient.ManifestResponse{{
					Manifests: []string{orphanTestConfigMap}, Namespace: test.FakeDestNamespace, Server: test.FakeClusterURL, Revision: "abc123",
				}},
				managedLiveObjs: map[kube.ResourceKey]*unstructured.Unstructured{},
			}, app)
			f.lookup["abc123"] = pinT1
			withMembership(t, f, members)

			f.refresh(t, app, CompareWithLatest)

			got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, v1alpha1.SyncStatusCodeOutOfSync, got.Status.Sync.Status, "still refreshes and shows status")
			assert.Nil(t, got.Operation, "starts no sync")
		})
	}
}

func TestMultihubWatchingOrNonMemberHubRefusesNewOperations(t *testing.T) {
	for name, tc := range map[string]struct{ members, msg string }{
		"watching":     {hubBWatching, "watching"},
		"not a member": {hubBNotAMember, "not a member"},
	} {
		t.Run(name, func(t *testing.T) {
			f, app := operationFixture(t)
			app.Operation.InitiatedBy = v1alpha1.OperationInitiator{Username: "alice"}
			withMembership(t, f, tc.members)

			f.ctrl.processRequestedAppOperation(app)

			got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.NotNil(t, got.Status.OperationState)
			assert.Equal(t, synccommon.OperationError, got.Status.OperationState.Phase)
			assert.Contains(t, got.Status.OperationState.Message, tc.msg)
			_, ok := f.override(t)
			assert.False(t, ok, "no side effects either (e.g. no rollback override)")
		})
	}
}

// Removed from membership mid-sync: the running sync finishes (the drain).
func TestMultihubNonMemberFinishesARunningOperation(t *testing.T) {
	f, app := operationFixture(t)
	app.Status.OperationState = &v1alpha1.OperationState{Operation: *app.Operation, Phase: synccommon.OperationRunning, StartedAt: metav1.Now()}
	withMembership(t, f, hubBNotAMember)

	f.ctrl.processRequestedAppOperation(app)

	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(t.Context(), app.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, synccommon.OperationSucceeded, got.Status.OperationState.Phase)
}

func TestMultihubWatchingHubNeitherClaimsNorPublishes(t *testing.T) {
	app := pinTestApp()
	f, _ := orphanFixture(t, app, &fakeData{apps: []runtime.Object{app, &defaultProj}})
	withMembership(t, f, hubBWatching)
	assert.False(t, f.ctrl.multihubClaimOrphan(t.Context(), app), "doesn't take over another hub's sync")

	other := pinTestApp()
	other.Name = "other"
	f.ctrl.multihubPublishRefresh(t.Context(), other, multihubSharedResult{Sync: v1alpha1.SyncStatus{Revision: "abc123"}})
	_, key, err := f.ctrl.multihubTarget(t.Context(), other)
	require.NoError(t, err)
	_, err = f.spoke.CoordinationV1().Leases(multihub.DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "publishes no shared refresh")
}

func TestMultihubRefreshPeriodComesFromHubMembershipFirst(t *testing.T) {
	app := pinTestApp()
	f := newMultihubPinFixture(t, &fakeData{apps: []runtime.Object{app, &defaultProj}, configMapData: map[string]string{"timeout.reconciliation": "10m"}}, app)
	require.Equal(t, 10*time.Minute, f.ctrl.refreshPeriod(), "no HubMembership: argocd-cm")

	withMembership(t, f, "    - {id: hub-b, state: active}\n")
	assert.Equal(t, 10*time.Minute, f.ctrl.refreshPeriod(), "HubMembership without refreshPeriod: argocd-cm")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(apiruntime.NewScheme(),
		map[schema.GroupVersionResource]string{multihub.HubMembershipGVR: "HubMembershipList"}, hubMembership(t, "    - {id: hub-b, state: active}\n"))
	f.ctrl.multihubMembership.Stop()
	f.ctrl.MultihubWatchMembership(dyn)
	withPeriod := hubMembership(t, "    - {id: hub-b, state: active}\n")
	require.NoError(t, unstructured.SetNestedField(withPeriod.Object, "3m", "spec", "refreshPeriod"))
	_, err := dyn.Resource(multihub.HubMembershipGVR).Namespace(test.FakeArgoCDNamespace).Update(t.Context(), withPeriod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return f.ctrl.refreshPeriod() == 3*time.Minute }, 10*time.Second, 20*time.Millisecond, "HubMembership's refreshPeriod wins")
}

// Found live (research/2026-11-22-hub-membership.md): the gate also stopped
// a watching hub's root app, so the Git change that makes it active again
// could never be applied. Apps that deploy into the hub's own cluster are
// that hub's own business; membership governs shared work only.
func TestMultihubWatchingHubStillManagesItsOwnCluster(t *testing.T) {
	root := pinTestApp()
	root.Name = "root"
	root.Spec.Destination = v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "argocd"}
	root.Operation = &v1alpha1.Operation{Sync: &v1alpha1.SyncOperation{}, InitiatedBy: v1alpha1.OperationInitiator{Automated: true}}
	f := newMultihubPinFixture(t, &fakeData{
		apps:              []runtime.Object{root, &defaultProj},
		manifestResponses: []*apiclient.ManifestResponse{{Manifests: []string{}}},
	}, root)
	withMembership(t, f, hubBWatching)

	ok, why := f.ctrl.multihubMayStart(t.Context(), root)
	assert.True(t, ok, why)

	f.ctrl.processRequestedAppOperation(root)
	got, err := f.ctrl.applicationClientset.ArgoprojV1alpha1().Applications(root.Namespace).Get(t.Context(), root.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Status.OperationState)
	// The unit-test controller can't build an in-cluster client, so the sync
	// itself can't succeed here; what matters is that it isn't refused.
	assert.NotContains(t, got.Status.OperationState.Message, "Operation not started", "its own root still syncs")

	shared := pinTestApp()
	ok, _ = f.ctrl.multihubMayStart(t.Context(), shared)
	assert.False(t, ok, "shared apps are still gated")
}
