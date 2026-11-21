package multihub

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
)

func cachedEventually(t *testing.T, c *Coordinator, cond func(LeaseView) bool) LeaseView {
	t.Helper()
	var got LeaseView
	require.Eventually(t, func() bool {
		v, ok := c.Cached(server, key)
		got = v
		return ok && cond(v)
	}, 10*time.Second, 20*time.Millisecond)
	return got
}

func TestCachedSeesAnotherHubsRefreshAndPin(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	t.Cleanup(b.StopWatches)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("abc123", t1))
	require.NoError(t, err)
	rec, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{}`))
	require.NoError(t, err)

	v := cachedEventually(t, b, func(v LeaseView) bool { return v.HasRefresh })
	assert.Equal(t, rec, v.Refresh)
	assert.True(t, v.HasPin)
	assert.Equal(t, "abc123", v.Pin.Revision)
}

func TestCachedFollowsUpdates(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	t.Cleanup(b.StopWatches)
	_, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{}`))
	require.NoError(t, err)
	cachedEventually(t, b, func(v LeaseView) bool { return v.HasRefresh })

	clk.advance(3 * time.Minute)
	rec, err := a.PublishRefresh(t.Context(), server, key, "def456", "fp-1", []byte(`{}`))
	require.NoError(t, err)
	v := cachedEventually(t, b, func(v LeaseView) bool { return v.Refresh.Revision == "def456" })
	assert.True(t, v.Refresh.At.Equal(rec.At))
}

func TestCachedIsNotOKBeforeTheWatchHasSynced(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	t.Cleanup(b.StopWatches)
	_, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{}`))
	require.NoError(t, err)
	_, ok := b.Cached(server, key)
	assert.False(t, ok, "the first call only starts the watch")
	cachedEventually(t, b, func(v LeaseView) bool { return v.HasRefresh })
}

func TestCachedWithoutTheLeaseIsNotOK(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	t.Cleanup(b.StopWatches)
	_, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{}`))
	require.NoError(t, err)
	cachedEventually(t, b, func(v LeaseView) bool { return v.HasRefresh })
	_, ok := b.Cached(server, Key{InstallationID: "lab", InstanceName: "other"})
	assert.False(t, ok)
}

func TestCachedIsUnhealthyAfterAWatchError(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	t.Cleanup(b.StopWatches)
	_, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{}`))
	require.NoError(t, err)
	spoke.PrependWatchReactor("leases", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, errors.New("connection refused")
	})

	_, _ = b.Cached(server, key)
	require.Eventually(t, func() bool { return b.watchSynced(server) }, 10*time.Second, 20*time.Millisecond, "listing works")
	require.Eventually(t, func() bool {
		_, ok := b.Cached(server, key)
		return !ok && !b.watchHealthy(server)
	}, 10*time.Second, 20*time.Millisecond, "the watch keeps failing: not trusted")
}

func TestDisabledCoordinatorCacheIsNeverOK(t *testing.T) {
	var c *Coordinator
	_, ok := c.Cached(server, key)
	assert.False(t, ok)
	c.StopWatches()
}
