package multihub

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hubARunning: hub-a holds the lease and has mirrored a running operation.
func hubARunning(t *testing.T) (a, b *Coordinator, gen int32, clk *clock) {
	t.Helper()
	a, b, _, clk = twoHubs(t)
	d, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	require.NoError(t, a.MirrorOperation(t.Context(), server, key, d.Generation, true, []byte(`{"phase":"Running"}`)))
	return a, b, d.Generation, clk
}

func TestMirrorRoundTrips(t *testing.T) {
	a, b, gen, _ := hubARunning(t)
	m, ok, err := b.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, OperationMirror{Hub: "hub-a", Generation: gen, Running: true, State: []byte(`{"phase":"Running"}`)}, m)

	require.NoError(t, a.MirrorOperation(t.Context(), server, key, gen, false, []byte(`{"phase":"Succeeded"}`)))
	m, _, err = b.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, m.Running)
	assert.Equal(t, []byte(`{"phase":"Succeeded"}`), m.State)
}

func TestMirrorIsCompressedAndKeepsOtherAnnotations(t *testing.T) {
	a, _, spoke, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	d, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	state := bytes.Repeat([]byte(`{"kind":"ConfigMap","status":"Synced"},`), 2000) // ~80KB, compresses well
	require.NoError(t, a.MirrorOperation(t.Context(), server, key, d.Generation, true, state))

	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	enc := lease.Annotations[AnnotationOperation]
	require.NotEmpty(t, enc)
	assert.Less(t, len(enc), len(state)/10, "gzipped")
	m, _, err := a.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, state, m.State)
	assert.Equal(t, "aaa111", lease.Annotations[AnnotationPinnedRevision], "pin kept")
}

func TestMirrorTooLargeIsRefused(t *testing.T) {
	a, _, _, _ := twoHubs(t)
	d, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	noise := make([]byte, MaxMirrorBytes) // random bytes don't compress
	_, _ = rand.Read(noise)
	assert.ErrorIs(t, a.MirrorOperation(t.Context(), server, key, d.Generation, true, noise), ErrMirrorTooLarge)
}

func TestMirrorIsFenced(t *testing.T) {
	a, b, gen, clk := hubARunning(t)

	// hub-b doesn't hold the lease.
	assert.ErrorIs(t, b.MirrorOperation(t.Context(), server, key, gen, true, []byte(`{}`)), ErrNotHolder)

	// hub-a's lease expires and hub-b claims the operation: hub-a's late
	// writes, at its old generation, must not overwrite hub-b's.
	clk.advance(61 * time.Second)
	c, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	require.Equal(t, ClaimClaimed, c.Event)
	assert.ErrorIs(t, a.MirrorOperation(t.Context(), server, key, gen, false, []byte(`{"stale":true}`)), ErrNotHolder)

	m, _, err := a.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, "hub-b", m.Hub)
	assert.Equal(t, c.Generation, m.Generation)
	assert.True(t, m.Running)
}

func TestClaimWaitsWhileHolderIsLive(t *testing.T) {
	_, b, _, clk := hubARunning(t)
	clk.advance(20 * time.Second)
	c, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimWaiting, c.Event)
	assert.Equal(t, 40*time.Second, c.RetryAfter)
	assert.Equal(t, "hub-a", c.Mirror.Hub)
}

func TestClaimTakesOverAnExpiredRunningOperation(t *testing.T) {
	_, b, gen, clk := hubARunning(t)
	clk.advance(61 * time.Second)
	c, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimClaimed, c.Event)
	assert.Equal(t, gen+1, c.Generation, "a new fencing token")
	assert.Equal(t, "hub-a", c.Mirror.Hub, "the mirror as hub-a left it")
	assert.Equal(t, []byte(`{"phase":"Running"}`), c.Mirror.State)

	// hub-b now holds the lease: its operation passes renew it.
	d, err := b.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, EventRenewed, d.Event)
	assert.Equal(t, c.Generation, d.Generation)
}

func TestClaimTakesOverARunningOperationReleasedWithoutFinishing(t *testing.T) {
	a, b, _, _ := hubARunning(t)
	require.NoError(t, a.Release(t.Context(), server, key))
	c, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimClaimed, c.Event)
}

func TestClaimIgnoresFinishedOwnAndMissing(t *testing.T) {
	a, b, gen, clk := hubARunning(t)

	c, err := a.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimNone, c.Event, "own operation: the operation processor resumes it")

	require.NoError(t, a.MirrorOperation(t.Context(), server, key, gen, false, []byte(`{"phase":"Succeeded"}`)))
	clk.advance(61 * time.Second)
	c, err = b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimNone, c.Event, "finished: nothing to take over")

	other := Key{InstallationID: "lab", InstanceName: "other"}
	c, err = b.ClaimOrphan(t.Context(), server, other)
	require.NoError(t, err)
	assert.Equal(t, ClaimNone, c.Event, "no lease")
	_, ok, err := b.CurrentOperation(t.Context(), server, other)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestClaimRaceHasOneWinner(t *testing.T) {
	a, b, _, clk := hubARunning(t)
	cfg := b.cfg
	cfg.HubID = "hub-c"
	c := New(cfg, b.clients)
	c.now = clk.now
	clk.advance(61 * time.Second)

	cb, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	cc, err := c.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimClaimed, cb.Event)
	assert.Equal(t, ClaimWaiting, cc.Event, "hub-b's claim re-mirrored the operation under hub-b, whose lease is live")
	assert.Equal(t, "hub-b", cc.Mirror.Hub)
	_ = a
}

func TestDisabledCoordinatorOperationCallsAreNoops(t *testing.T) {
	var c *Coordinator
	require.NoError(t, c.MirrorOperation(t.Context(), server, key, 1, true, []byte(`{}`)))
	_, ok, err := c.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)
	cl, err := c.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimNone, cl.Event)
}

// C3 gap 3: a claim whose Application write failed is given back, so the
// operation isn't stranded under a hub that will never claim it again.
func TestReturnClaimLetsTheOperationBeClaimedAgain(t *testing.T) {
	_, b, gen, clk := hubARunning(t)
	clk.advance(61 * time.Second)
	c, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	require.Equal(t, ClaimClaimed, c.Event)

	require.NoError(t, b.ReturnClaim(t.Context(), server, key, c))

	m, _, err := b.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, OperationMirror{Hub: "hub-a", Generation: gen, Running: true, State: []byte(`{"phase":"Running"}`)}, m, "mirror as hub-a left it")
	again, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimClaimed, again.Event, "claimable again, by this hub too")
}

func TestReturnClaimIsFenced(t *testing.T) {
	_, b, _, clk := hubARunning(t)
	clk.advance(61 * time.Second)
	c, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	stale := c
	stale.Generation--
	assert.ErrorIs(t, b.ReturnClaim(t.Context(), server, key, stale), ErrNotHolder)
	m, _, err := b.CurrentOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, "hub-b", m.Hub, "unchanged")
}

// C3 gap 5: passes that change nothing (e.g. every 5-30s while a hook runs)
// don't rewrite the mirror.
func TestMirrorSkipsTheWriteWhenUnchanged(t *testing.T) {
	a, _, spoke, _ := twoHubs(t)
	d, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	state := []byte(`{"phase":"Running","message":"waiting for completion of hook"}`)
	require.NoError(t, a.MirrorOperation(t.Context(), server, key, d.Generation, true, state))
	before := leaseUpdates(spoke.Actions())

	require.NoError(t, a.MirrorOperation(t.Context(), server, key, d.Generation, true, state))
	assert.Equal(t, before, leaseUpdates(spoke.Actions()), "same state: no write")

	require.NoError(t, a.MirrorOperation(t.Context(), server, key, d.Generation, false, state))
	assert.Equal(t, before+1, leaseUpdates(spoke.Actions()), "finished: written")
}

// E9 (research/2026-11-20-c3-gaps.md): hub-b's only refresh during hub-a's
// operation came right after hub-a took the lease and before its first
// mirror, so it saw "nothing running" and scheduled no refresh at expiry.
// A live lease held by another hub means "look again at expiry", mirror or
// not.
func TestClaimWaitsForALiveHolderBeforeItsFirstMirror(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	_, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	clk.advance(10 * time.Second)

	c, err := b.ClaimOrphan(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, ClaimWaiting, c.Event)
	assert.Equal(t, 50*time.Second, c.RetryAfter)
}
