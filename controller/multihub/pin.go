package multihub

// Revision pin (C1 in docs/proposals/multi-hub-active-active.md): the
// revision every hub compares and syncs an Application against. Stored as
// annotations on the Application's Lease on the destination cluster, so it
// shares the lease's compare-and-swap and needs no new object type.

import (
	"context"
	"time"
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
// revision all hubs should use. STUB (red): implemented in the next commit.
func (c *Coordinator) AgreeRevision(_ context.Context, _ string, _ Key, _ Candidate) (PinDecision, error) {
	return PinDecision{}, nil
}

// CurrentPin reads the pin without changing it. STUB (red).
func (c *Coordinator) CurrentPin(_ context.Context, _ string, _ Key) (Pin, bool, error) {
	return Pin{}, false, nil
}
