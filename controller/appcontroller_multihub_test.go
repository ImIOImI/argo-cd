package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/argoproj/argo-cd/gitops-engine/v3/pkg/utils/kube"

	"github.com/argoproj/argo-cd/v3/controller/multihub"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
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
