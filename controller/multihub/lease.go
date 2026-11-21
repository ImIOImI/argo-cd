// Package multihub coordinates several Argo CD installations ("hubs") that
// manage the same Applications on the same destination clusters.
//
// The shared channel is the destination cluster itself: one
// coordination.k8s.io/v1 Lease per Application, in a namespace on that
// cluster. Every hub can already write to the destination, and its API server
// gives compare-and-swap through resourceVersion, so no new datastore and no
// hub-to-hub link is needed. If a hub cannot reach the destination it cannot
// sync to it either, so the lock fails in the same way as the thing it guards.
//
// This file implements the operation lease (C2 in
// docs/proposals/multi-hub-active-active.md): at most one hub runs a sync
// operation for an Application at a time.
package multihub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/argoproj/argo-cd/v3/util/env"
)

const (
	EnvHubID         = "ARGOCD_MULTIHUB_HUB_ID"
	EnvNamespace     = "ARGOCD_MULTIHUB_NAMESPACE"
	EnvLeaseDuration = "ARGOCD_MULTIHUB_LEASE_DURATION"
	EnvSettleWindow  = "ARGOCD_MULTIHUB_SETTLE_WINDOW"
	// Share refreshes between hubs (C7): skip a periodic refresh another hub
	// just did. Default true.
	EnvShareRefresh = "ARGOCD_MULTIHUB_SHARE_REFRESH"

	DefaultNamespace     = "argocd-coordination"
	DefaultLeaseDuration = 60 * time.Second
	DefaultSettleWindow  = 10 * time.Second

	LabelManagedBy        = "app.kubernetes.io/managed-by"
	LabelManagedByValue   = "argocd-multihub"
	AnnotationApplication = "multihub.argoproj.io/application"
	AnnotationReleasedBy  = "multihub.argoproj.io/released-by"
	AnnotationReleasedAt  = "multihub.argoproj.io/released-at"

	maxConflictRetries = 5
)

// Config is per hub. Coordination is off unless HubID is set.
type Config struct {
	// HubID names this hub. Must be unique among hubs sharing destinations.
	HubID string
	// Namespace on each destination cluster that holds the Leases.
	Namespace string
	// LeaseDuration is how long a lease stays valid without a renewal. The
	// holder renews on every operation pass, which happens at least every
	// 30s (appOperationMaxRequeueInterval), so keep this comfortably above it.
	LeaseDuration time.Duration
	// SettleWindow: after another hub releases a lease, automated syncs on this
	// hub wait this long before acquiring it. A comparison computed while the
	// other hub's sync was still in flight is stale; the window lets this hub
	// re-compare against the finished state first (research R0).
	SettleWindow time.Duration
	// ShareRefresh: skip a periodic refresh that another hub just did and
	// show its result instead (C7).
	ShareRefresh bool
	// Telemetry controls logs, Kubernetes Events and metrics (telemetry.go).
	Telemetry Telemetry
}

func ConfigFromEnv() Config {
	return Config{
		HubID:         env.StringFromEnv(EnvHubID, ""),
		Namespace:     env.StringFromEnv(EnvNamespace, DefaultNamespace),
		LeaseDuration: env.ParseDurationFromEnv(EnvLeaseDuration, DefaultLeaseDuration, time.Second, time.Hour),
		SettleWindow:  env.ParseDurationFromEnv(EnvSettleWindow, DefaultSettleWindow, 0, time.Hour),
		ShareRefresh:  env.ParseBoolFromEnv(EnvShareRefresh, true),
		Telemetry:     TelemetryFromEnv(),
	}
}

// Reporter applies the telemetry policy; nil when disabled.
func (c *Coordinator) Reporter() *Reporter {
	if c == nil {
		return nil
	}
	return c.reporter
}

// ShareRefresh reports whether shared refresh (C7) is on.
func (c *Coordinator) ShareRefresh() bool { return c != nil && c.cfg.ShareRefresh }

// Telemetry returns the telemetry settings (defaults when disabled).
func (c *Coordinator) Telemetry() Telemetry {
	if c == nil {
		return DefaultTelemetry()
	}
	return c.cfg.Telemetry
}

// Key identifies an Application across hubs. Hubs that share an Application
// must agree on it; it mirrors the resource tracking identity
// (installationID + Application instance name), which is what already makes
// the destination resources "the same" to every hub.
type Key struct {
	InstallationID string
	InstanceName   string
}

func (k Key) String() string {
	if k.InstallationID == "" {
		return k.InstanceName
	}
	return k.InstallationID + "/" + k.InstanceName
}

// LeaseName is a DNS-1123 subdomain derived from the key. Readable when it
// fits, hashed when it does not; the full key is always in an annotation.
func (k Key) LeaseName() string {
	raw := strings.ToLower(strings.NewReplacer("/", ".", "_", ".").Replace(k.String()))
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, raw)
	clean = strings.Trim(clean, "-.")
	if clean != "" && len(clean) <= 200 && clean == raw {
		return clean
	}
	sum := sha256.Sum256([]byte(k.String()))
	prefix := clean
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return strings.Trim(prefix, "-.") + "-" + hex.EncodeToString(sum[:])[:16]
}

// Event classifies what happened to a lease. Used for logs, Kubernetes
// Events and the decision metric's label.
type Event string

const (
	EventAcquired        Event = "acquired"         // free (or new) lease taken
	EventRenewed         Event = "renewed"          // holder extended its own lease
	EventTakeover        Event = "takeover"         // another hub's lease had expired and was taken
	EventHeld            Event = "held"             // denied: another hub holds the lease
	EventSettling        Event = "settling"         // denied: another hub released it moments ago (auto-sync only)
	EventReleased        Event = "released"         // holder gave it up
	EventLost            Event = "lost"             // an in-progress operation found its lease taken over
	EventError           Event = "error"            // could not reach or update the lease
	EventReleaseError    Event = "release_error"    // could not release (it will expire)
	EventMirrorError     Event = "mirror_error"     // could not mirror operation state (C3); a takeover would restart it
	EventFollowing       Event = "following"        // this hub's operation was taken over by another hub; showing its state (C3)
	EventOverrideSet     Event = "override_set"     // a user's sync to a specific revision recorded the override (C5)
	EventOverrideCleared Event = "override_cleared" // a user's sync to the targetRevision cleared it (C5)
	EventOverridden      Event = "overridden"       // auto-sync skipped: an override is in place (C5)
	EventShared          Event = "shared"           // periodic refresh skipped: another hub's fresh result used (C7)
	EventDisabled        Event = "disabled"         // coordination is off
)

// Decision is the outcome of an acquire attempt.
type Decision struct {
	Event   Event
	Granted bool
	// Holder is the hub holding the lease when it was not granted, or the
	// hub that just released it when the settle window denied it.
	Holder string
	// Generation is the fencing token: it changes every time the lease
	// changes hands. Valid when Granted.
	Generation int32
	// RetryAfter is a hint for when asking again may succeed.
	RetryAfter time.Duration
	Reason     string
}

// ClientFunc returns a client for the destination cluster identified by server.
type ClientFunc func(server string) (kubernetes.Interface, error)

type Coordinator struct {
	cfg     Config
	clients ClientFunc
	now     func() time.Time

	nsMu      sync.Mutex
	nsEnsured map[string]bool

	reporter *Reporter

	watches leaseWatches
}

// New returns nil when coordination is disabled. All methods are safe on a
// nil *Coordinator and behave as "single hub": always granted.
func New(cfg Config, clients ClientFunc) *Coordinator {
	if cfg.HubID == "" {
		return nil
	}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = DefaultLeaseDuration
	}
	return &Coordinator{cfg: cfg, clients: clients, now: time.Now, nsEnsured: map[string]bool{}, reporter: NewReporter(cfg.Telemetry)}
}

func (c *Coordinator) Enabled() bool { return c != nil }

func (c *Coordinator) HubID() string {
	if c == nil {
		return ""
	}
	return c.cfg.HubID
}

// AcquireForOperation takes or renews the lease so this hub can start or
// continue a sync operation. A user-initiated operation is not subject to the
// settle window.
func (c *Coordinator) AcquireForOperation(ctx context.Context, server string, key Key) (Decision, error) {
	return c.acquire(ctx, server, key, false)
}

// AcquireForAutoSync is AcquireForOperation plus the settle window.
func (c *Coordinator) AcquireForAutoSync(ctx context.Context, server string, key Key) (Decision, error) {
	return c.acquire(ctx, server, key, true)
}

func (c *Coordinator) acquire(ctx context.Context, server string, key Key, settle bool) (Decision, error) {
	if c == nil {
		return Decision{Event: EventDisabled, Granted: true, Reason: "multihub disabled"}, nil
	}
	client, err := c.clients(server)
	if err != nil {
		return Decision{}, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	leases := client.CoordinationV1().Leases(c.cfg.Namespace)
	name := key.LeaseName()

	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		now := c.now()
		lease, err := leases.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := c.ensureNamespace(ctx, server, client); err != nil {
				return Decision{}, err
			}
			created, err := leases.Create(ctx, c.newLease(name, key, now), metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			if err != nil {
				return Decision{}, fmt.Errorf("multihub: create lease %s: %w", name, err)
			}
			return granted(created, EventAcquired, "acquired (new lease)"), nil
		}
		if err != nil {
			return Decision{}, fmt.Errorf("multihub: get lease %s: %w", name, err)
		}

		holder := holderOf(lease)
		switch {
		case holder == c.cfg.HubID:
			lease.Spec.RenewTime = micro(now)
			lease.Spec.LeaseDurationSeconds = seconds(c.cfg.LeaseDuration)
			updated, err := leases.Update(ctx, lease, metav1.UpdateOptions{})
			if apierrors.IsConflict(err) {
				continue
			}
			if err != nil {
				return Decision{}, fmt.Errorf("multihub: renew lease %s: %w", name, err)
			}
			return granted(updated, EventRenewed, "renewed"), nil

		case holder != "" && !c.expired(lease, now):
			return Decision{
				Event:      EventHeld,
				Holder:     holder,
				RetryAfter: c.remaining(lease, now),
				Reason:     "operation lease held by " + holder,
			}, nil
		}

		// Free (released) or expired.
		if settle && c.cfg.SettleWindow > 0 {
			by := lease.Annotations[AnnotationReleasedBy]
			if at, ok := parseTime(lease.Annotations[AnnotationReleasedAt]); ok && by != "" && by != c.cfg.HubID {
				if wait := at.Add(c.cfg.SettleWindow).Sub(now); wait > 0 {
					return Decision{
						Event:      EventSettling,
						Holder:     by,
						RetryAfter: wait,
						Reason:     fmt.Sprintf("settling after an operation by %s", by),
					}, nil
				}
			}
		}
		event, reason := EventAcquired, "acquired"
		if holder != "" {
			event, reason = EventTakeover, "acquired (lease held by "+holder+" expired)"
		}
		take(lease, c.cfg.HubID, c.cfg.LeaseDuration, now)
		updated, err := leases.Update(ctx, lease, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return Decision{}, fmt.Errorf("multihub: acquire lease %s: %w", name, err)
		}
		return granted(updated, event, reason), nil
	}
	return Decision{}, fmt.Errorf("multihub: lease %s: too many conflicting updates", name)
}

// Release gives the lease up if this hub holds it. It is not an error to
// release a lease held by someone else or one that does not exist.
func (c *Coordinator) Release(ctx context.Context, server string, key Key) error {
	if c == nil {
		return nil
	}
	client, err := c.clients(server)
	if err != nil {
		return fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	leases := client.CoordinationV1().Leases(c.cfg.Namespace)
	name := key.LeaseName()
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		lease, err := leases.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("multihub: get lease %s: %w", name, err)
		}
		if holderOf(lease) != c.cfg.HubID {
			return nil
		}
		empty := ""
		lease.Spec.HolderIdentity = &empty
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[AnnotationReleasedBy] = c.cfg.HubID
		lease.Annotations[AnnotationReleasedAt] = c.now().UTC().Format(time.RFC3339Nano)
		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("multihub: release lease %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("multihub: release lease %s: too many conflicting updates", name)
}

func (c *Coordinator) ensureNamespace(ctx context.Context, server string, client kubernetes.Interface) error {
	c.nsMu.Lock()
	done := c.nsEnsured[server]
	c.nsMu.Unlock()
	if done {
		return nil
	}
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   c.cfg.Namespace,
			Labels: map[string]string{LabelManagedBy: LabelManagedByValue},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("multihub: create namespace %s: %w", c.cfg.Namespace, err)
	}
	c.nsMu.Lock()
	c.nsEnsured[server] = true
	c.nsMu.Unlock()
	return nil
}

func (c *Coordinator) newLease(name string, key Key, now time.Time) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.cfg.Namespace,
			Labels:      map[string]string{LabelManagedBy: LabelManagedByValue},
			Annotations: map[string]string{AnnotationApplication: key.String()},
		},
	}
	take(lease, c.cfg.HubID, c.cfg.LeaseDuration, now)
	return lease
}

func (c *Coordinator) expired(lease *coordinationv1.Lease, now time.Time) bool {
	return c.remaining(lease, now) <= 0
}

func (c *Coordinator) remaining(lease *coordinationv1.Lease, now time.Time) time.Duration {
	if lease.Spec.RenewTime == nil {
		return 0
	}
	d := c.cfg.LeaseDuration
	if lease.Spec.LeaseDurationSeconds != nil {
		d = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}
	return lease.Spec.RenewTime.Add(d).Sub(now)
}

func take(lease *coordinationv1.Lease, hub string, d time.Duration, now time.Time) {
	var transitions int32
	if lease.Spec.LeaseTransitions != nil {
		transitions = *lease.Spec.LeaseTransitions
	}
	transitions++
	lease.Spec.HolderIdentity = &hub
	lease.Spec.AcquireTime = micro(now)
	lease.Spec.RenewTime = micro(now)
	lease.Spec.LeaseDurationSeconds = seconds(d)
	lease.Spec.LeaseTransitions = &transitions
}

func granted(lease *coordinationv1.Lease, event Event, reason string) Decision {
	var gen int32
	if lease.Spec.LeaseTransitions != nil {
		gen = *lease.Spec.LeaseTransitions
	}
	return Decision{Event: event, Granted: true, Holder: holderOf(lease), Generation: gen, Reason: reason}
}

func holderOf(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

func micro(t time.Time) *metav1.MicroTime {
	m := metav1.NewMicroTime(t)
	return &m
}

func seconds(d time.Duration) *int32 {
	s := int32(d / time.Second)
	return &s
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// ErrLeaseLost is returned to callers that held a lease and found another
// hub holding it, meaning their own lease expired and was taken over.
var ErrLeaseLost = errors.New("multihub: operation lease lost to another hub")
