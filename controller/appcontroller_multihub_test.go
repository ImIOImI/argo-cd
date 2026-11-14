package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
