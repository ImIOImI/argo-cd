package multihub

// Hub membership (see "Membership and voting" in
// docs/proposals/multi-hub-active-active.md): which hubs are members and in
// which state, declared in Git as a HubMembership resource
// (manifests/crds/hubmembership-crd.yaml) that every hub's root app syncs.

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// HubMembershipGVR identifies the HubMembership resource.
var HubMembershipGVR = schema.GroupVersionResource{Group: "multihub.argoproj.io", Version: "v1alpha1", Resource: "hubmemberships"}

// HubMembershipName is the one HubMembership object a hub reads.
const HubMembershipName = "default"

// MemberState is a member hub's declared state.
type MemberState string

const (
	// StateActive: votes and does the work.
	StateActive MemberState = "active"
	// StateWatching: reports its view but doesn't sync, share or vote.
	StateWatching MemberState = "watching"
)

// MemberEntry selects hubs by id or by label selector (exactly one).
type MemberEntry struct {
	ID       string
	Selector labels.Selector
	State    MemberState
}

// Membership is a parsed, validated HubMembership.
type Membership struct {
	// RefreshPeriod: the shared periodic refresh period; 0 = not set here.
	RefreshPeriod time.Duration
	// ForgetAfter: how long a silent member keeps counting; 0 = forever.
	ForgetAfter time.Duration
	Members     []MemberEntry
}

// ParseMembership validates a HubMembership object.
func ParseMembership(obj *unstructured.Unstructured) (Membership, error) {
	return Membership{}, nil
}

// ParseDuration parses "0" or a combination of <n>d, <n>h, <n>m and <n>s,
// e.g. "7d", "90m", "1d12h".
func ParseDuration(s string) (time.Duration, error) {
	return 0, nil
}
