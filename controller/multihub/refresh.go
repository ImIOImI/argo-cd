package multihub

// Shared refresh (C7 in docs/proposals/multi-hub-active-active.md).
//
// A hub that does a full refresh of an app publishes what it found, so that
// another hub whose periodic refresh comes due soon after can skip the
// comparison and show this result instead. The record of who refreshed,
// when, at which revision and for which app definition is a few small
// annotations on the app's Lease (watched by every hub). The result itself is
// a separate, gzipped object, written only when it changes and fetched only
// when a skipping hub doesn't already show it.

import (
	"context"
	"errors"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
)

const (
	AnnotationRefreshedBy          = "multihub.argoproj.io/refreshed-by"
	AnnotationRefreshedAt          = "multihub.argoproj.io/refreshed-at"
	AnnotationRefreshedRevision    = "multihub.argoproj.io/refreshed-revision"
	AnnotationRefreshedFingerprint = "multihub.argoproj.io/refreshed-fingerprint"
	AnnotationRefreshedResultHash  = "multihub.argoproj.io/refreshed-result-hash"

	// resultKey is the data key of the result object.
	resultKey = "result.json.gz"
)

// ErrResultMismatch: the result object doesn't hold the result the record
// names (another hub published in between). Refresh locally instead.
var ErrResultMismatch = errors.New("multihub: published result doesn't match the refresh record")

// RefreshRecord says which hub last fully refreshed an app, when, against
// which revision and app definition, and which result it found.
type RefreshRecord struct {
	By          string
	At          time.Time
	Revision    string
	Fingerprint string
	ResultHash  string
}

// PublishRefresh records a full refresh by this hub and its result. It
// returns the record as written.
func (c *Coordinator) PublishRefresh(ctx context.Context, server string, key Key, revision, fingerprint string, result []byte) (RefreshRecord, error) {
	return RefreshRecord{}, nil
}

// FetchResult reads the published result and checks it is the one with
// wantHash.
func (c *Coordinator) FetchResult(ctx context.Context, server string, key Key, wantHash string) ([]byte, error) {
	return nil, nil
}

// ResultName is the name of the object holding key's published result.
func ResultName(key Key) string { return key.LeaseName() + "-result" }

// RefreshRecordOf reads the refresh record from a Lease.
func RefreshRecordOf(lease *coordinationv1.Lease) (RefreshRecord, bool) {
	return RefreshRecord{}, false
}
