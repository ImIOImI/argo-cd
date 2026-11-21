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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// returns the record as written. The result object is written first, so a
// record never names a result that isn't there.
func (c *Coordinator) PublishRefresh(ctx context.Context, server string, key Key, revision, fingerprint string, result []byte) (RefreshRecord, error) {
	if c == nil {
		return RefreshRecord{}, nil
	}
	client, err := c.clients(server)
	if err != nil {
		return RefreshRecord{}, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	sum := sha256.Sum256(result)
	rec := RefreshRecord{By: c.cfg.HubID, Revision: revision, Fingerprint: fingerprint, ResultHash: hex.EncodeToString(sum[:16])}

	cms := client.CoreV1().ConfigMaps(c.cfg.Namespace)
	name := ResultName(key)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if err := c.ensureNamespace(ctx, server, client); err != nil {
			return RefreshRecord{}, err
		}
		data, err := gzipBytes(result)
		if err != nil {
			return RefreshRecord{}, err
		}
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   c.cfg.Namespace,
				Labels:      map[string]string{LabelManagedBy: LabelManagedByValue},
				Annotations: map[string]string{AnnotationApplication: key.String(), AnnotationRefreshedResultHash: rec.ResultHash},
			},
			BinaryData: map[string][]byte{resultKey: data},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return RefreshRecord{}, fmt.Errorf("multihub: create result %s: %w", name, err)
		}
	case err != nil:
		return RefreshRecord{}, fmt.Errorf("multihub: get result %s: %w", name, err)
	case cm.Annotations[AnnotationRefreshedResultHash] != rec.ResultHash:
		data, err := gzipBytes(result)
		if err != nil {
			return RefreshRecord{}, err
		}
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations[AnnotationRefreshedResultHash] = rec.ResultHash
		cm.BinaryData = map[string][]byte{resultKey: data}
		// Last writer wins; readers check the hash against the record.
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return RefreshRecord{}, fmt.Errorf("multihub: update result %s: %w", name, err)
		}
	}

	err = c.updateLeaseAnnotations(ctx, server, key, true, func(a map[string]string) bool {
		rec.At = c.now()
		a[AnnotationRefreshedBy] = rec.By
		a[AnnotationRefreshedAt] = rec.At.UTC().Format(time.RFC3339Nano)
		a[AnnotationRefreshedRevision] = rec.Revision
		a[AnnotationRefreshedFingerprint] = rec.Fingerprint
		a[AnnotationRefreshedResultHash] = rec.ResultHash
		return true
	})
	if err != nil {
		return RefreshRecord{}, err
	}
	return rec, nil
}

// FetchResult reads the published result and checks it is the one with
// wantHash.
func (c *Coordinator) FetchResult(ctx context.Context, server string, key Key, wantHash string) ([]byte, error) {
	if c == nil {
		return nil, ErrResultMismatch
	}
	client, err := c.clients(server)
	if err != nil {
		return nil, fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	cm, err := client.CoreV1().ConfigMaps(c.cfg.Namespace).Get(ctx, ResultName(key), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrResultMismatch
	}
	if err != nil {
		return nil, fmt.Errorf("multihub: get result %s: %w", ResultName(key), err)
	}
	if cm.Annotations[AnnotationRefreshedResultHash] != wantHash {
		return nil, ErrResultMismatch
	}
	zr, err := gzip.NewReader(bytes.NewReader(cm.BinaryData[resultKey]))
	if err != nil {
		return nil, fmt.Errorf("multihub: decode result %s: %w", ResultName(key), err)
	}
	return io.ReadAll(zr)
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ResultName is the name of the object holding key's published result.
func ResultName(key Key) string { return key.LeaseName() + "-result" }

// RefreshRecordOf reads the refresh record from a Lease.
func RefreshRecordOf(lease *coordinationv1.Lease) (RefreshRecord, bool) {
	a := lease.Annotations
	by := a[AnnotationRefreshedBy]
	if by == "" {
		return RefreshRecord{}, false
	}
	at, _ := parseTime(a[AnnotationRefreshedAt])
	return RefreshRecord{
		By:          by,
		At:          at,
		Revision:    a[AnnotationRefreshedRevision],
		Fingerprint: a[AnnotationRefreshedFingerprint],
		ResultHash:  a[AnnotationRefreshedResultHash],
	}, true
}
