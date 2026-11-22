package multihub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func fakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{HubMembershipGVR: "HubMembershipList"}, objs...)
}

func validMembership(t *testing.T, state string) *unstructured.Unstructured {
	t.Helper()
	return membershipObj(t, `
  members:
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: `+state+`
`)
}

func TestMembershipWatcherKeepsTheLastValidVersion(t *testing.T) {
	client := fakeDynamic(validMembership(t, "active"))
	w := NewMembershipWatcher(client, "argocd")
	t.Cleanup(w.Stop)

	require.Eventually(t, func() bool { _, ok := w.Current(); return ok }, 10*time.Second, 20*time.Millisecond)
	m, _ := w.Current()
	assert.Equal(t, StateActive, m.Members[0].State)
	require.NoError(t, w.LastError())

	// A valid change takes effect.
	_, err := client.Resource(HubMembershipGVR).Namespace("argocd").Update(t.Context(), validMembership(t, "watching"), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { m, _ := w.Current(); return m.Members[0].State == StateWatching }, 10*time.Second, 20*time.Millisecond)

	// An invalid one (the API server would normally reject it; e.g. a CRD
	// older than the fork) keeps the status quo and says why.
	bad := validMembership(t, "watching")
	require.NoError(t, unstructured.SetNestedSlice(bad.Object, []any{}, "spec", "members"))
	_, err = client.Resource(HubMembershipGVR).Namespace("argocd").Update(t.Context(), bad, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return w.LastError() != nil }, 10*time.Second, 20*time.Millisecond)
	m, ok := w.Current()
	assert.True(t, ok)
	assert.Equal(t, StateWatching, m.Members[0].State, "last valid version kept")

	// Removing the object returns to no declared membership.
	require.NoError(t, client.Resource(HubMembershipGVR).Namespace("argocd").Delete(t.Context(), HubMembershipName, metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { _, ok := w.Current(); return !ok }, 10*time.Second, 20*time.Millisecond)
	assert.NoError(t, w.LastError())
}

func TestMembershipWatcherIgnoresOtherObjects(t *testing.T) {
	other := validMembership(t, "active")
	other.SetName("not-default")
	w := NewMembershipWatcher(fakeDynamic(other), "argocd")
	t.Cleanup(w.Stop)
	time.Sleep(300 * time.Millisecond)
	_, ok := w.Current()
	assert.False(t, ok, "only the object named default counts")
}

func TestMembershipWatcherWithoutAnObject(t *testing.T) {
	w := NewMembershipWatcher(fakeDynamic(), "argocd")
	t.Cleanup(w.Stop)
	time.Sleep(300 * time.Millisecond)
	_, ok := w.Current()
	assert.False(t, ok)
	assert.NoError(t, w.LastError())
}
