package multihub

// Revision pin (C1 in docs/proposals/multi-hub-active-active.md): the
// revision every hub compares and syncs an Application against. Stored as
// annotations on the Application's Lease on the destination cluster, so it
// shares the lease's compare-and-swap and needs no new object type.
//
// Ordering uses commit time: the pin only moves to a candidate committed
// strictly later than the current pin. That is a spike approximation of
// "is a descendant of" (see research notes): it misorders force-pushes to an
// older commit and commits with skewed author clocks.

import (
	"context"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	AnnotationPinnedRevision   = "multihub.argoproj.io/pinned-revision"
	AnnotationPinnedBy         = "multihub.argoproj.io/pinned-by"
	AnnotationPinnedAt         = "multihub.argoproj.io/pinned-at"
	AnnotationPinnedCommitTime = "multihub.argoproj.io/pinned-commit-time"
)

// Pin is the agreed revision for an Application.
type Pin struct {
	Revision string
	// CommitTime orders revisions: the pin only moves to a later commit.
	CommitTime time.Time
	By         string
	At         time.Time
}

// Candidate is a revision a hub resolved, with its commit time.
type Candidate struct {
	Revision   string
	CommitTime time.Time
}

type PinEvent string

const (
	PinPinned   PinEvent = "pinned"   // no pin existed; the candidate became the pin
	PinKept     PinEvent = "kept"     // the candidate is the pin
	PinAdvanced PinEvent = "advanced" // the candidate is newer; the pin moved to it
	PinAdopted  PinEvent = "adopted"  // the pin is newer (or tied); use the pin, not the candidate
	PinDisabled PinEvent = "disabled" // coordination is off; the candidate passes through
)

type PinDecision struct {
	Event PinEvent
	// Pin is the revision to compare and sync against.
	Pin Pin
}

// AgreeRevision offers this hub's resolved revision and returns the
// revision all hubs should use. It writes only when the pin is created or
// moves forward; agreeing with or adopting the pin is a read.
func (c *Coordinator) AgreeRevision(ctx context.Context, server string, key Key, cand Candidate) (PinDecision, error) {
	if c == nil {
		return PinDecision{Event: PinDisabled, Pin: Pin{Revision: cand.Revision, CommitTime: cand.CommitTime}}, nil
	}
	if cand.Revision == "" {
		return PinDecision{}, errors.New("multihub: pin candidate has no revision")
	}
	if cand.CommitTime.IsZero() {
		return PinDecision{}, fmt.Errorf("multihub: pin candidate %s has no commit time", cand.Revision)
	}
	client, err := c.clients(server)
	if err != nil {
		return PinDecision{}, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	leases := client.CoordinationV1().Leases(c.cfg.Namespace)
	name := key.LeaseName()

	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		now := c.now()
		lease, err := leases.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := c.ensureNamespace(ctx, server, client); err != nil {
				return PinDecision{}, err
			}
			fresh := c.newPinLease(name, key)
			c.setPin(fresh, cand, now)
			created, err := leases.Create(ctx, fresh, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			if err != nil {
				return PinDecision{}, fmt.Errorf("multihub: create lease %s for pin: %w", name, err)
			}
			p, _ := pinOf(created)
			return PinDecision{Event: PinPinned, Pin: p}, nil
		}
		if err != nil {
			return PinDecision{}, fmt.Errorf("multihub: get lease %s: %w", name, err)
		}

		current, ok := pinOf(lease)
		var event PinEvent
		switch {
		case !ok:
			event = PinPinned
		case current.Revision == cand.Revision:
			return PinDecision{Event: PinKept, Pin: current}, nil
		case cand.CommitTime.After(current.CommitTime):
			event = PinAdvanced
		default:
			return PinDecision{Event: PinAdopted, Pin: current}, nil
		}

		c.setPin(lease, cand, now)
		updated, err := leases.Update(ctx, lease, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return PinDecision{}, fmt.Errorf("multihub: update pin on lease %s: %w", name, err)
		}
		p, _ := pinOf(updated)
		return PinDecision{Event: event, Pin: p}, nil
	}
	return PinDecision{}, fmt.Errorf("multihub: pin on lease %s: too many conflicting updates", name)
}

// CurrentPin reads the pin without changing it.
func (c *Coordinator) CurrentPin(ctx context.Context, server string, key Key) (Pin, bool, error) {
	if c == nil {
		return Pin{}, false, nil
	}
	client, err := c.clients(server)
	if err != nil {
		return Pin{}, false, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	lease, err := client.CoordinationV1().Leases(c.cfg.Namespace).Get(ctx, key.LeaseName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("multihub: get lease %s: %w", key.LeaseName(), err)
	}
	p, ok := pinOf(lease)
	return p, ok, nil
}

// newPinLease is a Lease that carries only a pin: no holder, so the first
// hub to need the operation lease takes it as free.
func (c *Coordinator) newPinLease(name string, key Key) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.cfg.Namespace,
			Labels:      map[string]string{LabelManagedBy: LabelManagedByValue},
			Annotations: map[string]string{AnnotationApplication: key.String()},
		},
	}
}

func (c *Coordinator) setPin(lease *coordinationv1.Lease, cand Candidate, now time.Time) {
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[AnnotationPinnedRevision] = cand.Revision
	lease.Annotations[AnnotationPinnedCommitTime] = cand.CommitTime.UTC().Format(time.RFC3339Nano)
	lease.Annotations[AnnotationPinnedBy] = c.cfg.HubID
	lease.Annotations[AnnotationPinnedAt] = now.UTC().Format(time.RFC3339Nano)
}

func pinOf(lease *coordinationv1.Lease) (Pin, bool) {
	rev := lease.Annotations[AnnotationPinnedRevision]
	if rev == "" {
		return Pin{}, false
	}
	commitTime, _ := parseTime(lease.Annotations[AnnotationPinnedCommitTime])
	at, _ := parseTime(lease.Annotations[AnnotationPinnedAt])
	return Pin{Revision: rev, CommitTime: commitTime, By: lease.Annotations[AnnotationPinnedBy], At: at}, true
}
