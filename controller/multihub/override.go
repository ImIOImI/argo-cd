package multihub

// Coordinated manual operations (C5 in docs/proposals/multi-hub-active-active.md).
//
// A user-initiated sync to a specific revision (a rollback, or a sync to a
// revision other than the app's targetRevision) records an override on the
// Application's Lease. While an override exists, no hub starts an automated
// sync for the app: a single Argo CD refuses a rollback while auto-sync is
// on, and with several hubs every hub's auto-sync has to stand down, or the
// others revert the rollback (research E5). A user sync to the
// targetRevision on any hub clears it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	AnnotationOverrideRevision = "multihub.argoproj.io/override-revision"
	AnnotationOverrideBy       = "multihub.argoproj.io/override-by"
	AnnotationOverrideAt       = "multihub.argoproj.io/override-at"
	AnnotationOverrideReason   = "multihub.argoproj.io/override-reason"
)

// Override is a manual pin: automated syncs stand down until it's cleared.
type Override struct {
	Revision string
	By       string
	At       time.Time
	Reason   string
}

// SetOverride records an override by this hub, replacing any existing one.
func (c *Coordinator) SetOverride(ctx context.Context, server string, key Key, revision, reason string) (Override, error) {
	if c == nil {
		return Override{}, nil
	}
	if revision == "" {
		return Override{}, errors.New("multihub: override has no revision")
	}
	o := Override{Revision: revision, By: c.cfg.HubID, Reason: reason}
	err := c.updateLeaseAnnotations(ctx, server, key, true, func(a map[string]string) bool {
		o.At = c.now()
		a[AnnotationOverrideRevision] = o.Revision
		a[AnnotationOverrideBy] = o.By
		a[AnnotationOverrideAt] = o.At.UTC().Format(time.RFC3339Nano)
		a[AnnotationOverrideReason] = o.Reason
		return true
	})
	if err != nil {
		return Override{}, err
	}
	return o, nil
}

// ClearOverride removes the override, if any, and returns the one removed.
func (c *Coordinator) ClearOverride(ctx context.Context, server string, key Key) (Override, bool, error) {
	if c == nil {
		return Override{}, false, nil
	}
	var removed Override
	var found bool
	err := c.updateLeaseAnnotations(ctx, server, key, false, func(a map[string]string) bool {
		removed, found = overrideOf(a)
		if !found {
			return false
		}
		for _, k := range []string{AnnotationOverrideRevision, AnnotationOverrideBy, AnnotationOverrideAt, AnnotationOverrideReason} {
			delete(a, k)
		}
		return true
	})
	return removed, found, err
}

// CurrentOverride reads the override without changing it.
func (c *Coordinator) CurrentOverride(ctx context.Context, server string, key Key) (Override, bool, error) {
	if c == nil {
		return Override{}, false, nil
	}
	client, err := c.clients(server)
	if err != nil {
		return Override{}, false, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	lease, err := client.CoordinationV1().Leases(c.cfg.Namespace).Get(ctx, key.LeaseName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Override{}, false, nil
	}
	if err != nil {
		return Override{}, false, fmt.Errorf("multihub: get lease %s: %w", key.LeaseName(), err)
	}
	o, ok := overrideOf(lease.Annotations)
	return o, ok, nil
}

// updateLeaseAnnotations applies change to the app Lease's annotations with
// compare-and-swap, retrying on conflicts. change returns false for "no
// write needed". create: make a holder-less Lease if there is none.
func (c *Coordinator) updateLeaseAnnotations(ctx context.Context, server string, key Key, create bool, change func(map[string]string) bool) error {
	client, err := c.clients(server)
	if err != nil {
		return fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	leases := client.CoordinationV1().Leases(c.cfg.Namespace)
	name := key.LeaseName()
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		lease, err := leases.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if !create {
				return nil
			}
			if err := c.ensureNamespace(ctx, server, client); err != nil {
				return err
			}
			fresh := c.newPinLease(name, key)
			change(fresh.Annotations)
			_, err = leases.Create(ctx, fresh, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("multihub: create lease %s: %w", name, err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("multihub: get lease %s: %w", name, err)
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		if !change(lease.Annotations) {
			return nil
		}
		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("multihub: update lease %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("multihub: lease %s: too many conflicting updates", name)
}

func overrideOf(a map[string]string) (Override, bool) {
	rev := a[AnnotationOverrideRevision]
	if rev == "" {
		return Override{}, false
	}
	at, _ := parseTime(a[AnnotationOverrideAt])
	return Override{Revision: rev, By: a[AnnotationOverrideBy], At: at, Reason: a[AnnotationOverrideReason]}, true
}
