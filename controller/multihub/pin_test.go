package multihub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

var (
	t1 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	t2 = t1.Add(time.Minute)
	t3 = t1.Add(2 * time.Minute)
)

func cand(rev string, at time.Time) Candidate { return Candidate{Revision: rev, CommitTime: at} }

func leaseUpdates(actions []k8stesting.Action) int {
	n := 0
	for _, a := range actions {
		if a.GetVerb() == "update" && a.GetResource().Resource == "leases" {
			n++
		}
	}
	return n
}

func TestPinFirstCandidateIsPinned(t *testing.T) {
	a, _, spoke, _ := twoHubs(t)
	d, err := a.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	assert.Equal(t, PinPinned, d.Event)
	assert.Equal(t, "aaa111", d.Pin.Revision)
	assert.Equal(t, "hub-a", d.Pin.By)
	assert.True(t, d.Pin.CommitTime.Equal(t1))

	// Stored on the app's Lease, without taking the operation lease.
	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "aaa111", lease.Annotations[AnnotationPinnedRevision])
	assert.Empty(t, holderOf(lease), "pinning must not take the operation lease")

	p, ok, err := a.CurrentPin(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "aaa111", p.Revision)
}

func TestPinSameRevisionIsKeptWithoutWriting(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	before := leaseUpdates(spoke.Actions())

	d, err := b.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	assert.Equal(t, PinKept, d.Event)
	assert.Equal(t, "aaa111", d.Pin.Revision)
	assert.Equal(t, "hub-a", d.Pin.By)
	assert.Equal(t, before, leaseUpdates(spoke.Actions()), "agreeing with the pin must not write")
}

func TestNewerCandidateAdvancesPin(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)

	d, err := b.AgreeRevision(t.Context(), server, key, cand("bbb222", t2))
	require.NoError(t, err)
	assert.Equal(t, PinAdvanced, d.Event)
	assert.Equal(t, "bbb222", d.Pin.Revision)
	assert.Equal(t, "hub-b", d.Pin.By)

	p, _, err := a.CurrentPin(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, "bbb222", p.Revision)
}

// The E2 flap: hub-a has pinned the new commit; hub-b still resolves the old
// one. hub-b must adopt the pin, not move it back.
func TestOlderCandidateAdoptsPin(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("bbb222", t2))
	require.NoError(t, err)
	before := leaseUpdates(spoke.Actions())

	d, err := b.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	assert.Equal(t, PinAdopted, d.Event)
	assert.Equal(t, "bbb222", d.Pin.Revision, "a stale hub must adopt the newer pin")
	assert.Equal(t, "hub-a", d.Pin.By)
	assert.Equal(t, before, leaseUpdates(spoke.Actions()), "adopting must not write")
}

func TestEqualCommitTimeKeepsExistingPin(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("aaa111", t2))
	require.NoError(t, err)
	d, err := b.AgreeRevision(t.Context(), server, key, cand("ccc333", t2))
	require.NoError(t, err)
	assert.Equal(t, PinAdopted, d.Event, "ties go to the existing pin (first writer wins)")
	assert.Equal(t, "aaa111", d.Pin.Revision)
}

func TestPinAdvancesMonotonically(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	for _, step := range []struct {
		hub  *Coordinator
		c    Candidate
		want string
	}{
		{a, cand("aaa111", t1), "aaa111"},
		{b, cand("ccc333", t3), "ccc333"},
		{a, cand("bbb222", t2), "ccc333"}, // late arrival of an older commit
		{b, cand("aaa111", t1), "ccc333"},
	} {
		d, err := step.hub.AgreeRevision(t.Context(), server, key, step.c)
		require.NoError(t, err)
		assert.Equal(t, step.want, d.Pin.Revision, "after %s offered %s", step.hub.HubID(), step.c.Revision)
	}
}

func TestPinDoesNotDisturbOperationLease(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	held, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)

	_, err = b.AgreeRevision(t.Context(), server, key, cand("bbb222", t2))
	require.NoError(t, err)

	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "hub-a", holderOf(lease))
	assert.Equal(t, held.Generation, *lease.Spec.LeaseTransitions, "pinning must not move the fencing token")

	renewed, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.Equal(t, EventRenewed, renewed.Event)
}

func TestLeaseOperationsPreservePin(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("bbb222", t2))
	require.NoError(t, err)

	_, err = b.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	_, err = b.AcquireForOperation(t.Context(), server, key) // renew
	require.NoError(t, err)
	require.NoError(t, b.Release(t.Context(), server, key))
	clk.advance(2 * time.Minute)
	_, err = a.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)

	p, ok, err := a.CurrentPin(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "bbb222", p.Revision)
	assert.True(t, p.CommitTime.Equal(t2))
}

func TestPinOnExistingLeaseCreatedByOperation(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	_, err := a.AcquireForOperation(t.Context(), server, key) // lease exists, no pin yet
	require.NoError(t, err)
	d, err := b.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	assert.Equal(t, PinPinned, d.Event)
	assert.Equal(t, "aaa111", d.Pin.Revision)
}

func TestNoPinYet(t *testing.T) {
	a, _, _, _ := twoHubs(t)
	_, ok, err := a.CurrentPin(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPinRequiresRevisionAndCommitTime(t *testing.T) {
	a, _, _, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("", t1))
	require.Error(t, err)
	_, err = a.AgreeRevision(t.Context(), server, key, Candidate{Revision: "aaa111"})
	require.Error(t, err, "ordering needs a commit time")
}

func TestPinRetriesOnConflict(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("aaa111", t1))
	require.NoError(t, err)

	conflicts := 1
	spoke.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, key.LeaseName(), nil)
		}
		return false, nil, nil
	})
	d, err := b.AgreeRevision(t.Context(), server, key, cand("bbb222", t2))
	require.NoError(t, err)
	assert.Equal(t, PinAdvanced, d.Event)
	assert.Equal(t, 0, conflicts, "the conflicting update must have been retried")
}

func TestDisabledCoordinatorPassesCandidateThrough(t *testing.T) {
	var c *Coordinator
	d, err := c.AgreeRevision(context.Background(), server, key, cand("aaa111", t1))
	require.NoError(t, err)
	assert.Equal(t, PinDisabled, d.Event)
	assert.Equal(t, "aaa111", d.Pin.Revision)
	_, ok, err := c.CurrentPin(context.Background(), server, key)
	require.NoError(t, err)
	assert.False(t, ok)
}
