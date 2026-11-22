package multihub

// Hub membership (see "Membership and voting" in
// docs/proposals/multi-hub-active-active.md): which hubs are members and in
// which state, declared in Git as a HubMembership resource
// (manifests/crds/hubmembership-crd.yaml) that every hub's root app syncs.

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
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
	spec, ok, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !ok {
		return Membership{}, errors.New("hubmembership: no spec")
	}
	var m Membership
	if v, ok := spec["refreshPeriod"].(string); ok {
		d, err := ParseDuration(v)
		if err != nil {
			return Membership{}, fmt.Errorf("hubmembership: refreshPeriod: %w", err)
		}
		if d == 0 {
			return Membership{}, errors.New("hubmembership: refreshPeriod must be more than 0")
		}
		m.RefreshPeriod = d
	} else if _, set := spec["refreshPeriod"]; set {
		return Membership{}, errors.New("hubmembership: refreshPeriod must be a duration string")
	}
	switch v := spec["forgetAfter"].(type) {
	case nil:
	case string:
		d, err := ParseDuration(v)
		if err != nil {
			return Membership{}, fmt.Errorf("hubmembership: forgetAfter: %w", err)
		}
		m.ForgetAfter = d
	case int64, float64, int:
		// `forgetAfter: 0` unquoted is a YAML number; only 0 is allowed so.
		if fmt.Sprint(v) != "0" {
			return Membership{}, errors.New("hubmembership: forgetAfter must be 0 or a duration string (e.g. 7d)")
		}
	default:
		return Membership{}, errors.New("hubmembership: forgetAfter must be 0 or a duration string (e.g. 7d)")
	}
	raw, _ := spec["members"].([]any)
	if len(raw) == 0 {
		return Membership{}, errors.New("hubmembership: members must list at least one entry")
	}
	ids := map[string]bool{}
	for i, r := range raw {
		e, ok := r.(map[string]any)
		if !ok {
			return Membership{}, fmt.Errorf("hubmembership: members[%d] is not an object", i)
		}
		var entry MemberEntry
		switch st := MemberState(fmt.Sprint(e["state"])); st {
		case StateActive, StateWatching:
			entry.State = st
		default:
			return Membership{}, fmt.Errorf("hubmembership: members[%d].state must be active or watching", i)
		}
		id, hasID := e["id"].(string)
		sel, hasSel := e["selector"].(map[string]any)
		if hasID == hasSel || (hasID && id == "") {
			return Membership{}, fmt.Errorf("hubmembership: members[%d] needs exactly one of id or selector", i)
		}
		if hasID {
			if ids[id] {
				return Membership{}, fmt.Errorf("hubmembership: members[%d]: id %q listed twice", i, id)
			}
			ids[id] = true
			entry.ID = id
		} else {
			var ls metav1.LabelSelector
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(sel, &ls); err != nil {
				return Membership{}, fmt.Errorf("hubmembership: members[%d].selector: %w", i, err)
			}
			selector, err := metav1.LabelSelectorAsSelector(&ls)
			if err != nil {
				return Membership{}, fmt.Errorf("hubmembership: members[%d].selector: %w", i, err)
			}
			entry.Selector = selector
		}
		m.Members = append(m.Members, entry)
	}
	return m, nil
}

var durationPart = regexp.MustCompile(`([0-9]+)([dhms])`)

// ParseDuration parses "0" or a combination of <n>d, <n>h, <n>m and <n>s,
// e.g. "7d", "90m", "1d12h".
func ParseDuration(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	if s == "" || durationPart.ReplaceAllString(s, "") != "" {
		return 0, fmt.Errorf("invalid duration %q: use 0 or combinations of <n>d, <n>h, <n>m, <n>s (e.g. 7d, 1d12h, 90m)", s)
	}
	units := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second}
	var total time.Duration
	for _, p := range durationPart.FindAllStringSubmatch(s, -1) {
		n, err := strconv.ParseInt(p[1], 10, 64)
		if err != nil || n > int64(math.MaxInt64/units[p[2]]) {
			return 0, fmt.Errorf("invalid duration %q: too large", s)
		}
		part := time.Duration(n) * units[p[2]]
		if total > math.MaxInt64-part {
			return 0, fmt.Errorf("invalid duration %q: too large", s)
		}
		total += part
	}
	return total, nil
}

// HubRecord is what a hub publishes about itself on each spoke.
type HubRecord struct {
	ID       string
	Labels   map[string]string
	LastSeen time.Time
}

// Resolved is a membership resolved against the hub records seen.
type Resolved struct {
	// States of the member hubs by id; non-members are absent.
	States map[string]MemberState
	// Active member ids, sorted.
	Active []string
	// Quorum: how many active members must agree.
	Quorum int
}

// StateOf is the declared state of the hub with id and labels; ok is false
// if no entry matches it (not a member).
func (m Membership) StateOf(id string, lbls map[string]string) (state MemberState, ok bool, err error) {
	for _, e := range m.Members {
		if e.ID != "" && e.ID == id {
			return e.State, true, nil // an id entry overrides selectors
		}
	}
	set := labels.Set(lbls)
	for _, e := range m.Members {
		if e.Selector == nil || !e.Selector.Matches(set) {
			continue
		}
		if ok && e.State != state {
			return "", false, fmt.Errorf("hubmembership: hub %s matches selectors with different states (%s and %s)", id, state, e.State)
		}
		state, ok = e.State, true
	}
	return state, ok, nil
}

// Resolve resolves the membership against records, forgetting members
// silent for longer than ForgetAfter (if set) as of now.
func (m Membership) Resolve(records []HubRecord, now time.Time) (Resolved, error) {
	r := Resolved{States: map[string]MemberState{}}
	for _, rec := range records {
		if m.ForgetAfter > 0 && now.Sub(rec.LastSeen) > m.ForgetAfter {
			continue
		}
		st, ok, err := m.StateOf(rec.ID, rec.Labels)
		if err != nil {
			return Resolved{}, err
		}
		if !ok {
			continue
		}
		r.States[rec.ID] = st
		if st == StateActive {
			r.Active = append(r.Active, rec.ID)
		}
	}
	// Hubs declared by id count even without a record: strictest reading.
	for _, e := range m.Members {
		if e.ID == "" {
			continue
		}
		if _, seen := r.States[e.ID]; seen {
			continue
		}
		if m.ForgetAfter > 0 && recordedAndForgotten(records, e.ID, now, m.ForgetAfter) {
			continue
		}
		r.States[e.ID] = e.State
		if e.State == StateActive {
			r.Active = append(r.Active, e.ID)
		}
	}
	sort.Strings(r.Active)
	r.Quorum = QuorumFor(len(r.Active))
	return r, nil
}

// recordedAndForgotten: id has a record, silent for longer than forget.
func recordedAndForgotten(records []HubRecord, id string, now time.Time, forget time.Duration) bool {
	for _, rec := range records {
		if rec.ID == id {
			return now.Sub(rec.LastSeen) > forget
		}
	}
	return false
}

// QuorumFor is how many of n active members must agree: all of them with
// one or two, a majority with three or more.
func QuorumFor(n int) int {
	if n <= 2 {
		return n
	}
	return n/2 + 1
}
