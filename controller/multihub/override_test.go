package multihub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOverrideSetIsSeenByEveryHub(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	o, err := b.SetOverride(t.Context(), server, key, "e264214", "rollback to history 0 by alice")
	require.NoError(t, err)
	assert.Equal(t, Override{Revision: "e264214", By: "hub-b", At: clk.t, Reason: "rollback to history 0 by alice"}, o)

	got, ok, err := a.CurrentOverride(t.Context(), server, key)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, o, got)
}

func TestOverrideKeepsTheLeaseAndPin(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	d, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)

	_, err = b.SetOverride(t.Context(), server, key, "e264214", "rollback")
	require.NoError(t, err)

	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "aaa111", lease.Annotations[AnnotationPinnedRevision])
	assert.Equal(t, "hub-a", holderOf(lease), "an override is not the operation lease")
	assert.Equal(t, d.Generation, generationOf(lease))
	assert.Equal(t, "e264214", lease.Annotations[AnnotationOverrideRevision])
}

func TestOverrideReplacesAndClears(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	_, err := b.SetOverride(t.Context(), server, key, "e264214", "rollback")
	require.NoError(t, err)
	clk.advance(time.Minute)
	_, err = a.SetOverride(t.Context(), server, key, "ded013c", "sync to ded013c by bob")
	require.NoError(t, err)
	got, _, err := b.CurrentOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, "ded013c", got.Revision)
	assert.Equal(t, "hub-a", got.By)

	removed, ok, err := b.ClearOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "ded013c", removed.Revision)
	_, ok, err = a.CurrentOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok, err = a.ClearOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok, "nothing to clear")
}

func TestOverrideOnAMissingLease(t *testing.T) {
	a, _, _, _ := twoHubs(t)
	_, ok, err := a.CurrentOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = a.ClearOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = a.SetOverride(t.Context(), server, key, "e264214", "rollback")
	require.NoError(t, err, "creates the lease (without a holder)")
	_, ok, err = a.CurrentOverride(t.Context(), server, key)
	require.NoError(t, err)
	require.True(t, ok)
	d, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, EventAcquired, d.Event, "the lease is free")
}

func TestOverrideRejectsAnEmptyRevision(t *testing.T) {
	a, _, _, _ := twoHubs(t)
	_, err := a.SetOverride(t.Context(), server, key, "", "rollback")
	assert.Error(t, err)
}

func TestDisabledCoordinatorOverrideCallsAreNoops(t *testing.T) {
	var c *Coordinator
	_, err := c.SetOverride(t.Context(), server, key, "x", "y")
	require.NoError(t, err)
	_, ok, err := c.CurrentOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = c.ClearOverride(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)
}
