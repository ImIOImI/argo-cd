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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// AnnotationOperation holds the mirrored operation (encodeMirror).
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
	if c == nil {
		return nil
	}
	enc, err := encodeMirror(OperationMirror{Hub: c.cfg.HubID, Generation: generation, Running: running, State: state})
	if err != nil {
		return err
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
			return ErrNotHolder
		}
		if err != nil {
			return fmt.Errorf("multihub: get lease %s: %w", name, err)
		}
		if holderOf(lease) != c.cfg.HubID || generationOf(lease) != generation {
			return ErrNotHolder
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[AnnotationOperation] = enc
		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("multihub: mirror operation on lease %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("multihub: lease %s: too many conflicting updates", name)
}

// CurrentOperation reads the mirrored operation, if any.
func (c *Coordinator) CurrentOperation(ctx context.Context, server string, key Key) (OperationMirror, bool, error) {
	if c == nil {
		return OperationMirror{}, false, nil
	}
	client, err := c.clients(server)
	if err != nil {
		return OperationMirror{}, false, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	lease, err := client.CoordinationV1().Leases(c.cfg.Namespace).Get(ctx, key.LeaseName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return OperationMirror{}, false, nil
	}
	if err != nil {
		return OperationMirror{}, false, fmt.Errorf("multihub: get lease %s: %w", key.LeaseName(), err)
	}
	return mirrorOf(lease)
}

// ClaimOrphan takes over another hub's running operation once that hub's
// lease has expired (or was released without finishing).
func (c *Coordinator) ClaimOrphan(ctx context.Context, server string, key Key) (Claim, error) {
	if c == nil {
		return Claim{Event: ClaimNone}, nil
	}
	client, err := c.clients(server)
	if err != nil {
		return Claim{}, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	leases := client.CoordinationV1().Leases(c.cfg.Namespace)
	name := key.LeaseName()
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		now := c.now()
		lease, err := leases.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return Claim{Event: ClaimNone}, nil
		}
		if err != nil {
			return Claim{}, fmt.Errorf("multihub: get lease %s: %w", name, err)
		}
		m, ok, err := mirrorOf(lease)
		if err != nil {
			return Claim{}, err
		}
		holder := holderOf(lease)
		// Not ours to claim: nothing mirrored, finished, our own (the
		// operation processor resumes it), or we already hold the lease.
		if !ok || !m.Running || m.Hub == c.cfg.HubID || holder == c.cfg.HubID {
			return Claim{Event: ClaimNone, Mirror: m}, nil
		}
		if holder != "" && !c.expired(lease, now) {
			return Claim{Event: ClaimWaiting, Mirror: m, RetryAfter: c.remaining(lease, now)}, nil
		}
		take(lease, c.cfg.HubID, c.cfg.LeaseDuration, now)
		gen := generationOf(lease)
		claimed := m
		claimed.Hub, claimed.Generation = c.cfg.HubID, gen
		enc, err := encodeMirror(claimed)
		if err != nil {
			return Claim{}, err
		}
		lease.Annotations[AnnotationOperation] = enc
		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return Claim{}, fmt.Errorf("multihub: claim lease %s: %w", name, err)
		}
		return Claim{Event: ClaimClaimed, Mirror: m, Generation: gen}, nil
	}
	return Claim{}, fmt.Errorf("multihub: lease %s: too many conflicting updates", name)
}

// mirrorHeader is OperationMirror without the state. The encoded form is
// base64(gzip(header JSON + "\n" + state)), so the state is stored as-is.
type mirrorHeader struct {
	Hub        string `json:"hub"`
	Generation int32  `json:"generation"`
	Running    bool   `json:"running"`
}

func encodeMirror(m OperationMirror) (string, error) {
	h, err := json.Marshal(mirrorHeader{Hub: m.Hub, Generation: m.Generation, Running: m.Running})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(h)
	_, _ = zw.Write([]byte("\n"))
	_, _ = zw.Write(m.State)
	if err := zw.Close(); err != nil {
		return "", err
	}
	enc := base64.StdEncoding.EncodeToString(buf.Bytes())
	if len(enc) > MaxMirrorBytes {
		return "", fmt.Errorf("%w: %d bytes encoded, limit %d", ErrMirrorTooLarge, len(enc), MaxMirrorBytes)
	}
	return enc, nil
}

func mirrorOf(lease *coordinationv1.Lease) (OperationMirror, bool, error) {
	enc := lease.Annotations[AnnotationOperation]
	if enc == "" {
		return OperationMirror{}, false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return OperationMirror{}, false, fmt.Errorf("multihub: decode operation mirror: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return OperationMirror{}, false, fmt.Errorf("multihub: decode operation mirror: %w", err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return OperationMirror{}, false, fmt.Errorf("multihub: decode operation mirror: %w", err)
	}
	head, state, _ := bytes.Cut(data, []byte("\n"))
	var h mirrorHeader
	if err := json.Unmarshal(head, &h); err != nil {
		return OperationMirror{}, false, fmt.Errorf("multihub: decode operation mirror: %w", err)
	}
	return OperationMirror{Hub: h.Hub, Generation: h.Generation, Running: h.Running, State: state}, true, nil
}

func generationOf(lease *coordinationv1.Lease) int32 {
	if lease.Spec.LeaseTransitions == nil {
		return 0
	}
	return *lease.Spec.LeaseTransitions
}
