package multihub

// Operation takeover (C3 in docs/proposals/multi-hub-active-active.md).
//
// The hub running a sync operation mirrors its operation state onto the
// Application's Lease after every pass. If that hub dies, its lease expires
// and another hub claims the operation: it takes the lease (a new
// generation) and resumes from the mirrored state. Resuming with the same
// revision and startedAt makes gitops-engine compute the same generated hook
// names, so a hook that is still running is adopted, not started again.
//
// The package treats the state as opaque bytes; the controller decides what
// goes in it.

import (
	"context"
	"errors"
	"time"
)

const (
	// AnnotationOperation holds the mirrored operation: OperationMirror as
	// JSON, gzipped, base64-encoded.
	AnnotationOperation = "multihub.argoproj.io/operation"
	// MaxMirrorBytes caps the encoded annotation. Annotations on one object
	// share a 256KiB limit, which the pin and lease annotations also use.
	MaxMirrorBytes = 200 * 1024
)

var (
	// ErrNotHolder: the mirror wasn't written because this hub doesn't hold
	// the lease at the given generation (fencing).
	ErrNotHolder = errors.New("multihub: not the operation lease holder at this generation")
	// ErrMirrorTooLarge: the encoded state exceeds MaxMirrorBytes; the
	// operation can't be taken over and would restart instead.
	ErrMirrorTooLarge = errors.New("multihub: operation state too large to mirror")
)

// OperationMirror is an operation as last recorded by the hub running it.
type OperationMirror struct {
	// Hub that recorded it.
	Hub string `json:"hub"`
	// Generation of the lease it was recorded under.
	Generation int32 `json:"generation"`
	// Running: the operation hasn't completed.
	Running bool `json:"running"`
	// State is the controller's serialized operation state.
	State []byte `json:"state"`
}

type ClaimEvent string

const (
	ClaimNone    ClaimEvent = "none"    // nothing to take over
	ClaimWaiting ClaimEvent = "waiting" // another hub's running operation; its lease is still live
	ClaimClaimed ClaimEvent = "claimed" // took over the lease; resume Mirror.State
)

// Claim is the outcome of ClaimOrphan.
type Claim struct {
	Event ClaimEvent
	// Mirror as found (for claimed: as recorded by the previous hub).
	Mirror OperationMirror
	// Generation of this hub's lease, when claimed.
	Generation int32
	// RetryAfter, when waiting: when the other hub's lease expires.
	RetryAfter time.Duration
}

// MirrorOperation records this hub's operation state on the lease. It only
// writes while this hub holds the lease at generation.
func (c *Coordinator) MirrorOperation(ctx context.Context, server string, key Key, generation int32, running bool, state []byte) error {
	return nil
}

// CurrentOperation reads the mirrored operation, if any.
func (c *Coordinator) CurrentOperation(ctx context.Context, server string, key Key) (OperationMirror, bool, error) {
	return OperationMirror{}, false, nil
}

// ClaimOrphan takes over another hub's running operation once that hub's
// lease has expired (or was released without finishing).
func (c *Coordinator) ClaimOrphan(ctx context.Context, server string, key Key) (Claim, error) {
	return Claim{Event: ClaimNone}, nil
}
