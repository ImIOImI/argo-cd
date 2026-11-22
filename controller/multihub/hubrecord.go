package multihub

// Hub records: each hub publishes on every destination cluster who it is
// (id, labels from its self-registration) and when it was last seen, as a
// Lease named hub.<id> in the coordination namespace. Membership is resolved
// from these records (see Membership.Resolve), so a hub that goes silent
// still counts until forgetAfter.

import (
	"context"
	"encoding/json"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	AnnotationHubID     = "multihub.argoproj.io/hub-id"
	AnnotationHubLabels = "multihub.argoproj.io/hub-labels"
	LabelRecord         = "multihub.argoproj.io/record"
	LabelRecordHub      = "hub"
)

// HubRecordName is the Lease name of hub id's record.
func HubRecordName(id string) string { return "hub." + Key{InstanceName: id}.LeaseName() }

// PublishHubRecord writes (or refreshes) this hub's record on server.
func (c *Coordinator) PublishHubRecord(ctx context.Context, server string, labels map[string]string) error {
	if c == nil {
		return nil
	}
	client, err := c.clients(server)
	if err != nil {
		return fmt.Errorf("multihub: client for %s: %w", server, err)
	}
	raw, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	leases := client.CoordinationV1().Leases(c.cfg.Namespace)
	name := HubRecordName(c.cfg.HubID)
	id := c.cfg.HubID
	now := micro(c.now())
	lease, err := leases.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err := c.ensureNamespace(ctx, server, client); err != nil {
			return err
		}
		_, err = leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   c.cfg.Namespace,
				Labels:      map[string]string{LabelManagedBy: LabelManagedByValue, LabelRecord: LabelRecordHub},
				Annotations: map[string]string{AnnotationHubID: id, AnnotationHubLabels: string(raw)},
			},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &id, RenewTime: now},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("multihub: create hub record %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("multihub: get hub record %s: %w", name, err)
	}
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[AnnotationHubID] = id
	lease.Annotations[AnnotationHubLabels] = string(raw)
	lease.Spec.HolderIdentity = &id
	lease.Spec.RenewTime = now
	// Only this hub writes its record; a conflict just means try next beat.
	if _, err := leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("multihub: update hub record %s: %w", name, err)
	}
	return nil
}

// HubRecords lists the hub records on server from the Lease watch cache; ok
// is false if the cache isn't ready or healthy.
func (c *Coordinator) HubRecords(server string) ([]HubRecord, bool) {
	if c == nil {
		return nil, false
	}
	w := c.watch(server)
	if w == nil || !c.healthy(w) {
		return nil, false
	}
	var out []HubRecord
	for _, obj := range w.informer.GetStore().List() {
		lease, ok := obj.(*coordinationv1.Lease)
		if !ok || lease.Labels[LabelRecord] != LabelRecordHub || lease.Annotations[AnnotationHubID] == "" {
			continue
		}
		rec := HubRecord{ID: lease.Annotations[AnnotationHubID]}
		_ = json.Unmarshal([]byte(lease.Annotations[AnnotationHubLabels]), &rec.Labels)
		if lease.Spec.RenewTime != nil {
			rec.LastSeen = lease.Spec.RenewTime.Time
		}
		out = append(out, rec)
	}
	return out, true
}
