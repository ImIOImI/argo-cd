package multihub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func recordsEventually(t *testing.T, c *Coordinator, n int) []HubRecord {
	t.Helper()
	var got []HubRecord
	require.Eventually(t, func() bool {
		r, ok := c.HubRecords(server)
		got = r
		return ok && len(r) == n
	}, 10*time.Second, 20*time.Millisecond)
	return got
}

func TestHubRecordsArePublishedAndSeenByOtherHubs(t *testing.T) {
	a, b, spoke, clk := twoHubs(t)
	t.Cleanup(b.StopWatches)
	require.NoError(t, a.PublishHubRecord(t.Context(), server, map[string]string{"multihub.argoproj.io/role": "hub", "multihub.argoproj.io/hub-id": "hub-a"}))
	require.NoError(t, b.PublishHubRecord(t.Context(), server, map[string]string{"multihub.argoproj.io/role": "hub"}))
	_, err := a.AgreeRevision(t.Context(), server, key, cand("abc123", t1)) // an app Lease: not a hub record
	require.NoError(t, err)

	recs := recordsEventually(t, b, 2)
	byID := map[string]HubRecord{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	assert.Equal(t, "hub", byID["hub-a"].Labels["multihub.argoproj.io/role"])
	assert.Equal(t, "hub-a", byID["hub-a"].Labels["multihub.argoproj.io/hub-id"])
	assert.True(t, byID["hub-a"].LastSeen.Equal(clk.t))

	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), HubRecordName("hub-a"), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "hub-a", lease.Annotations[AnnotationHubID])
	assert.Equal(t, LabelRecordHub, lease.Labels[LabelRecord])
}

func TestHubRecordLastSeenMovesOnRepublish(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	t.Cleanup(b.StopWatches)
	require.NoError(t, a.PublishHubRecord(t.Context(), server, nil))
	recordsEventually(t, b, 1)
	clk.advance(30 * time.Second)
	require.NoError(t, a.PublishHubRecord(t.Context(), server, map[string]string{"x": "y"}))
	require.Eventually(t, func() bool {
		r, _ := b.HubRecords(server)
		return len(r) == 1 && r[0].LastSeen.Equal(clk.t) && r[0].Labels["x"] == "y"
	}, 10*time.Second, 20*time.Millisecond)
}

func TestDisabledCoordinatorHubRecords(t *testing.T) {
	var c *Coordinator
	require.NoError(t, c.PublishHubRecord(t.Context(), server, nil))
	_, ok := c.HubRecords(server)
	assert.False(t, ok)
}
