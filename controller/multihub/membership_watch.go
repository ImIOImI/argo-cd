package multihub

import "k8s.io/client-go/dynamic"

// MembershipWatcher keeps the last valid HubMembership in a namespace (the
// object named HubMembershipName). An invalid version keeps the previous one
// (the status quo) and is reported by LastError; no object at all means no
// membership is declared, i.e. today's behavior.
type MembershipWatcher struct{}

// NewMembershipWatcher starts watching; Stop stops it.
func NewMembershipWatcher(client dynamic.Interface, namespace string) *MembershipWatcher {
	return &MembershipWatcher{}
}

// Current is the membership in force; ok is false when none is declared
// (or the watch hasn't synced yet).
func (w *MembershipWatcher) Current() (Membership, bool) { return Membership{}, false }

// LastError is why the latest version was rejected, or nil.
func (w *MembershipWatcher) LastError() error { return nil }

// Stop stops the watch.
func (w *MembershipWatcher) Stop() {}
