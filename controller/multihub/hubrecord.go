package multihub

// Hub records: each hub publishes on every destination cluster who it is
// (id, labels from its self-registration) and when it was last seen, as a
// Lease named hub.<id> in the coordination namespace. Membership is resolved
// from these records (see Membership.Resolve), so a hub that goes silent
// still counts until forgetAfter.

import "context"

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
	return nil
}

// HubRecords lists the hub records on server from the Lease watch cache; ok
// is false if the cache isn't ready or healthy.
func (c *Coordinator) HubRecords(server string) ([]HubRecord, bool) {
	return nil, false
}
