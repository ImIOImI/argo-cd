package multihub

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"
)

func configMapWrites(actions []k8stesting.Action) int {
	n := 0
	for _, a := range actions {
		if (a.GetVerb() == "update" || a.GetVerb() == "create") && a.GetResource().Resource == "configmaps" {
			n++
		}
	}
	return n
}

func TestPublishRefreshRecordsOnTheLeaseAndStoresTheResult(t *testing.T) {
	a, b, spoke, clk := twoHubs(t)
	result := []byte(`{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}`)

	rec, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", result)
	require.NoError(t, err)
	assert.Equal(t, "hub-a", rec.By)
	assert.True(t, rec.At.Equal(clk.t))
	assert.Equal(t, "abc123", rec.Revision)
	assert.Equal(t, "fp-1", rec.Fingerprint)
	assert.NotEmpty(t, rec.ResultHash)

	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	got, ok := RefreshRecordOf(lease)
	require.True(t, ok)
	assert.Equal(t, rec, got)
	assert.Empty(t, holderOf(lease), "publishing doesn't take the operation lease")
	for _, v := range lease.Annotations {
		assert.NotContains(t, v, "Healthy", "the result itself stays off the watched Lease")
	}

	fetched, err := b.FetchResult(t.Context(), server, key, rec.ResultHash)
	require.NoError(t, err)
	assert.Equal(t, result, fetched)
}

func TestPublishRefreshRewritesTheResultOnlyWhenItChanges(t *testing.T) {
	a, _, spoke, clk := twoHubs(t)
	result := []byte(`{"sync":{"status":"Synced"}}`)
	_, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", result)
	require.NoError(t, err)
	writes := configMapWrites(spoke.Actions())

	clk.advance(3 * time.Minute)
	rec, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", result)
	require.NoError(t, err)
	assert.Equal(t, writes, configMapWrites(spoke.Actions()), "same result: object not rewritten")
	assert.True(t, rec.At.Equal(clk.t), "the record still says when")

	changed, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{"sync":{"status":"OutOfSync"}}`))
	require.NoError(t, err)
	assert.Equal(t, writes+1, configMapWrites(spoke.Actions()))
	assert.NotEqual(t, rec.ResultHash, changed.ResultHash)
}

func TestPublishRefreshCompressesAndKeepsOtherAnnotations(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	_, err := a.AgreeRevision(t.Context(), server, key, cand("abc123", t1))
	require.NoError(t, err)
	_, err = b.SetOverride(t.Context(), server, key, "e264214", "rollback")
	require.NoError(t, err)
	big := bytes.Repeat([]byte(`{"kind":"ConfigMap","status":"Synced","health":"Healthy"},`), 3000)

	_, err = a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", big)
	require.NoError(t, err)

	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "abc123", lease.Annotations[AnnotationPinnedRevision])
	assert.Equal(t, "e264214", lease.Annotations[AnnotationOverrideRevision])
	cm, err := spoke.CoreV1().ConfigMaps(DefaultNamespace).Get(t.Context(), ResultName(key), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Less(t, len(cm.BinaryData[resultKey]), len(big)/10, "gzipped")
	assert.Equal(t, LabelManagedByValue, cm.Labels[LabelManagedBy])
}

func TestFetchResultRejectsAResultThatIsNotTheRecordedOne(t *testing.T) {
	a, b, _, _ := twoHubs(t)
	first, err := a.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{"v":1}`))
	require.NoError(t, err)
	_, err = b.PublishRefresh(t.Context(), server, key, "abc123", "fp-1", []byte(`{"v":2}`))
	require.NoError(t, err)

	_, err = a.FetchResult(t.Context(), server, key, first.ResultHash)
	assert.ErrorIs(t, err, ErrResultMismatch)
}

func TestFetchResultWithNothingPublished(t *testing.T) {
	a, _, _, _ := twoHubs(t)
	_, err := a.FetchResult(t.Context(), server, key, "deadbeef")
	assert.ErrorIs(t, err, ErrResultMismatch)
}

func TestRefreshRecordOfALeaseWithoutOne(t *testing.T) {
	a, _, spoke, _ := twoHubs(t)
	_, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	_, ok := RefreshRecordOf(lease)
	assert.False(t, ok)
}

func TestDisabledCoordinatorRefreshCallsAreNoops(t *testing.T) {
	var c *Coordinator
	_, err := c.PublishRefresh(t.Context(), server, key, "r", "f", []byte(`{}`))
	require.NoError(t, err)
	_, err = c.FetchResult(t.Context(), server, key, "h")
	assert.ErrorIs(t, err, ErrResultMismatch)
}

func TestShareRefreshFromEnv(t *testing.T) {
	assert.True(t, ConfigFromEnv().ShareRefresh, "on by default")
	t.Setenv(EnvShareRefresh, "false")
	assert.False(t, ConfigFromEnv().ShareRefresh)
	t.Setenv(EnvHubID, "hub-x")
	t.Setenv(EnvShareRefresh, "true")
	assert.True(t, New(ConfigFromEnv(), nil).ShareRefresh())
	var off *Coordinator
	assert.False(t, off.ShareRefresh())
}
