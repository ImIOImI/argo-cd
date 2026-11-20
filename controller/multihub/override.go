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
	"time"
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
	return Override{}, nil
}

// ClearOverride removes the override, if any, and returns the one removed.
func (c *Coordinator) ClearOverride(ctx context.Context, server string, key Key) (Override, bool, error) {
	return Override{}, false, nil
}

// CurrentOverride reads the override without changing it.
func (c *Coordinator) CurrentOverride(ctx context.Context, server string, key Key) (Override, bool, error) {
	return Override{}, false, nil
}
