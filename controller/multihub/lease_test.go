package multihub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const server = "https://spoke:6443"

var key = Key{InstallationID: "lab", InstanceName: "lab-app"}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// twoHubs returns coordinators for hub-a and hub-b sharing one fake spoke and one clock.
func twoHubs(t *testing.T) (a, b *Coordinator, spoke *fake.Clientset, clk *clock) {
	t.Helper()
	spoke = fake.NewClientset()
	clk = &clock{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	clients := func(string) (kubernetes.Interface, error) { return spoke, nil }
	mk := func(id string) *Coordinator {
		c := New(Config{HubID: id, LeaseDuration: 60 * time.Second, SettleWindow: 10 * time.Second}, clients)
		c.now = clk.now
		return c
	}
	return mk("hub-a"), mk("hub-b"), spoke, clk
}

func TestDisabledCoordinatorAlwaysGrants(t *testing.T) {
	var c *Coordinator = New(Config{}, nil)
	assert.False(t, c.Enabled())
	d, err := c.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, d.Granted)
	require.NoError(t, c.Release(t.Context(), server, key))
}

func TestFirstAcquireCreatesNamespaceAndLease(t *testing.T) {
	a, _, spoke, _ := twoHubs(t)
	d, err := a.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, d.Granted)
	assert.Equal(t, int32(1), d.Generation)

	_, err = spoke.CoreV1().Namespaces().Get(t.Context(), DefaultNamespace, metav1.GetOptions{})
	require.NoError(t, err)
	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "hub-a", *lease.Spec.HolderIdentity)
	assert.Equal(t, "lab/lab-app", lease.Annotations[AnnotationApplication])
}

func TestOtherHubIsDeniedWhileHeld(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	_, err := a.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)

	clk.advance(20 * time.Second)
	for _, acquire := range []func(context.Context, string, Key) (Decision, error){b.AcquireForAutoSync, b.AcquireForOperation} {
		d, err := acquire(t.Context(), server, key)
		require.NoError(t, err)
		assert.False(t, d.Granted)
		assert.Equal(t, "hub-a", d.Holder)
		assert.Equal(t, 40*time.Second, d.RetryAfter)
	}
}

func TestHolderRenewsWithoutChangingGeneration(t *testing.T) {
	a, _, _, clk := twoHubs(t)
	d1, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	clk.advance(50 * time.Second)
	d2, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, d2.Granted)
	assert.Equal(t, d1.Generation, d2.Generation)
	assert.Equal(t, "renewed", d2.Reason)
}

func TestExpiredLeaseIsTakenOverWithNewGeneration(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	d1, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)

	clk.advance(61 * time.Second) // hub-a stopped renewing (dead or partitioned)
	d2, err := b.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, d2.Granted)
	assert.Greater(t, d2.Generation, d1.Generation, "fencing token must move on takeover")
	assert.Contains(t, d2.Reason, "expired")

	// hub-a comes back: it no longer holds the lease.
	d3, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, d3.Granted)
	assert.Equal(t, "hub-b", d3.Holder)
}

func TestSettleWindowDelaysAutoSyncButNotUserOperations(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	_, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	clk.advance(5 * time.Second)
	require.NoError(t, a.Release(t.Context(), server, key))

	clk.advance(3 * time.Second)
	d, err := b.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)
	assert.False(t, d.Granted, "auto-sync must wait for the settle window")
	assert.Equal(t, "hub-a", d.Holder)
	assert.Equal(t, 7*time.Second, d.RetryAfter)

	d, err = b.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, d.Granted, "a user operation is not delayed")
}

func TestSettleWindowDoesNotApplyToOwnRelease(t *testing.T) {
	a, _, _, clk := twoHubs(t)
	_, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	require.NoError(t, a.Release(t.Context(), server, key))
	clk.advance(time.Second)
	d, err := a.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, d.Granted)
}

func TestSettleWindowEnds(t *testing.T) {
	a, b, _, clk := twoHubs(t)
	_, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	require.NoError(t, a.Release(t.Context(), server, key))
	clk.advance(11 * time.Second)
	d, err := b.AcquireForAutoSync(t.Context(), server, key)
	require.NoError(t, err)
	assert.True(t, d.Granted)
	assert.Equal(t, int32(2), d.Generation)
}

func TestReleaseByNonHolderIsANoop(t *testing.T) {
	a, b, spoke, _ := twoHubs(t)
	_, err := a.AcquireForOperation(t.Context(), server, key)
	require.NoError(t, err)
	require.NoError(t, b.Release(t.Context(), server, key))
	lease, err := spoke.CoordinationV1().Leases(DefaultNamespace).Get(t.Context(), key.LeaseName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "hub-a", *lease.Spec.HolderIdentity)
	require.NoError(t, b.Release(t.Context(), server, Key{InstanceName: "never-created"}))
}

func TestLeaseNames(t *testing.T) {
	assert.Equal(t, "lab.lab-app", Key{InstallationID: "lab", InstanceName: "lab-app"}.LeaseName())
	assert.Equal(t, "lab-app", Key{InstanceName: "lab-app"}.LeaseName())
	assert.Equal(t, "lab.team-a.web", Key{InstallationID: "lab", InstanceName: "team-a_web"}.LeaseName())

	odd := Key{InstallationID: "My Install", InstanceName: "app"}.LeaseName()
	assert.Regexp(t, `^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`, odd)
	assert.NotEqual(t, odd, Key{InstallationID: "My-Install", InstanceName: "app"}.LeaseName(), "distinct keys must not collide")

	long := Key{InstanceName: strings.Repeat("a", 250)}.LeaseName()
	assert.LessOrEqual(t, len(long), 63)
}
