package multihub

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

func membershipObj(t *testing.T, spec string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal([]byte(`
apiVersion: multihub.argoproj.io/v1alpha1
kind: HubMembership
metadata: {name: default, namespace: argocd}
spec:
`+spec), &obj.Object))
	return obj
}

func TestParseMembership(t *testing.T) {
	m, err := ParseMembership(membershipObj(t, `
  refreshPeriod: 120s
  forgetAfter: 1d12h
  members:
    - selector:
        matchLabels: {multihub.argoproj.io/role: hub}
      state: active
    - id: hub-c
      state: watching
    - selector:
        matchExpressions:
          - {key: region, operator: In, values: [us-east, us-west]}
      state: watching
`))
	require.NoError(t, err)
	assert.Equal(t, 120*time.Second, m.RefreshPeriod)
	assert.Equal(t, 36*time.Hour, m.ForgetAfter)
	require.Len(t, m.Members, 3)
	assert.Equal(t, StateActive, m.Members[0].State)
	assert.True(t, m.Members[0].Selector.Matches(labels.Set{"multihub.argoproj.io/role": "hub"}))
	assert.False(t, m.Members[0].Selector.Matches(labels.Set{"role": "spoke"}))
	assert.Equal(t, "hub-c", m.Members[1].ID)
	assert.Nil(t, m.Members[1].Selector)
	assert.True(t, m.Members[2].Selector.Matches(labels.Set{"region": "us-west"}))
}

func TestParseMembershipDefaults(t *testing.T) {
	m, err := ParseMembership(membershipObj(t, `
  members:
    - id: hub-a
      state: active
`))
	require.NoError(t, err)
	assert.Zero(t, m.RefreshPeriod, "not set here: fall back")
	assert.Zero(t, m.ForgetAfter, "default: never forget a silent member")
}

func TestParseMembershipRejects(t *testing.T) {
	cases := map[string]string{
		"no members":        "\n  members: []\n",
		"unknown state":     "\n  members:\n    - {id: hub-a, state: paused}\n",
		"no state":          "\n  members:\n    - {id: hub-a}\n",
		"id and selector":   "\n  members:\n    - {id: hub-a, selector: {matchLabels: {a: b}}, state: active}\n",
		"neither":           "\n  members:\n    - {state: active}\n",
		"bad selector":      "\n  members:\n    - {selector: {matchExpressions: [{key: a, operator: Sometimes}]}, state: active}\n",
		"duplicate id":      "\n  members:\n    - {id: hub-a, state: active}\n    - {id: hub-a, state: watching}\n",
		"bad forgetAfter":   "\n  forgetAfter: a week\n  members:\n    - {id: hub-a, state: active}\n",
		"bad refreshPeriod": "\n  refreshPeriod: soon\n  members:\n    - {id: hub-a, state: active}\n",
		"zero refreshPeriod (disable it upstream-style, not here)": "\n  refreshPeriod: 0\n  members:\n    - {id: hub-a, state: active}\n",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMembership(membershipObj(t, spec))
			assert.Error(t, err)
		})
	}
	_, err := ParseMembership(&unstructured.Unstructured{Object: map[string]any{"kind": "HubMembership"}})
	assert.Error(t, err, "no spec")
}

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"0": 0, "90m": 90 * time.Minute, "12h": 12 * time.Hour, "7d": 7 * 24 * time.Hour,
		"1d12h": 36 * time.Hour, "2h30m": 150 * time.Minute, "120s": 2 * time.Minute, "1d0h5m": 24*time.Hour + 5*time.Minute,
	}
	for s, want := range ok {
		got, err := ParseDuration(s)
		require.NoError(t, err, s)
		assert.Equal(t, want, got, s)
	}
	for _, s := range []string{"", "a week", "7", "7w", "-1d", "1.5h", "d", "1d 12h", "99999999999999999999d"} {
		_, err := ParseDuration(s)
		assert.Error(t, err, s)
	}
}

// The CRD shipped with the fork validates the same rules at apply time.
func TestHubMembershipCRD(t *testing.T) {
	raw, err := os.ReadFile("../../manifests/crds/hubmembership-crd.yaml")
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(raw, &crd))
	assert.Equal(t, "hubmemberships.multihub.argoproj.io", crd.Name)
	assert.Equal(t, HubMembershipGVR.Group, crd.Spec.Group)
	assert.Equal(t, apiextensionsv1.NamespaceScoped, crd.Spec.Scope)
	require.Len(t, crd.Spec.Versions, 1)
	v := crd.Spec.Versions[0]
	assert.Equal(t, HubMembershipGVR.Version, v.Name)
	assert.NotNil(t, v.Subresources.Status, "status subresource: each hub's view")
	spec := v.Schema.OpenAPIV3Schema.Properties["spec"]
	assert.Contains(t, spec.Required, "members")
	members := spec.Properties["members"]
	require.NotNil(t, members.MinItems)
	assert.Equal(t, int64(1), *members.MinItems)
	item := members.Items.Schema
	var states []string
	for _, e := range item.Properties["state"].Enum {
		states = append(states, string(e.Raw))
	}
	assert.ElementsMatch(t, []string{`"active"`, `"watching"`}, states)
	require.NotEmpty(t, item.XValidations, "exactly one of id/selector")
	assert.Contains(t, item.XValidations[0].Rule, "has(self.id)")
	assert.NotEmpty(t, spec.Properties["forgetAfter"].Pattern)
	assert.NotEmpty(t, spec.Properties["refreshPeriod"].Pattern)
}

func TestQuorumFor(t *testing.T) {
	for n, want := range map[int]int{1: 1, 2: 2, 3: 2, 4: 3, 5: 3, 6: 4, 7: 4} {
		assert.Equal(t, want, QuorumFor(n), "%d active", n)
	}
	assert.Equal(t, 0, QuorumFor(0))
}

func exampleMembership(t *testing.T, forget string) Membership {
	t.Helper()
	m, err := ParseMembership(membershipObj(t, `
  forgetAfter: `+forget+`
  members:
    - selector:
        matchLabels: {multihub.argoproj.io/role: hub}
      state: active
    - id: hub-c
      state: watching
`))
	require.NoError(t, err)
	return m
}

var hubLabels = map[string]string{"multihub.argoproj.io/role": "hub"}

func TestStateOf(t *testing.T) {
	m := exampleMembership(t, "0")
	st, ok, err := m.StateOf("hub-a", hubLabels)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, StateActive, st)

	st, ok, err = m.StateOf("hub-c", hubLabels)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, StateWatching, st, "an id entry overrides the selector")

	_, ok, err = m.StateOf("hub-x", map[string]string{"role": "spoke"})
	require.NoError(t, err)
	assert.False(t, ok, "matches nothing: not a member")
}

func TestStateOfConflictingSelectors(t *testing.T) {
	m, err := ParseMembership(membershipObj(t, `
  members:
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: active
    - selector: {matchLabels: {region: eu-west}}
      state: watching
    - selector: {matchLabels: {tier: gold}}
      state: active
`))
	require.NoError(t, err)
	_, _, err = m.StateOf("hub-a", map[string]string{"multihub.argoproj.io/role": "hub", "region": "eu-west"})
	assert.Error(t, err, "two selectors, two states: a conflict")
	st, ok, err := m.StateOf("hub-a", map[string]string{"multihub.argoproj.io/role": "hub", "tier": "gold"})
	require.NoError(t, err, "two selectors, same state: fine")
	assert.True(t, ok)
	assert.Equal(t, StateActive, st)
}

func TestResolve(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	records := []HubRecord{
		{ID: "hub-a", Labels: hubLabels, LastSeen: now},
		{ID: "hub-b", Labels: hubLabels, LastSeen: now.Add(-8 * 24 * time.Hour)}, // silent 8 days
		{ID: "hub-c", Labels: hubLabels, LastSeen: now},
		{ID: "hub-d", Labels: hubLabels, LastSeen: now.Add(-time.Minute)},
		{ID: "spoke-tool", Labels: map[string]string{"role": "other"}, LastSeen: now},
	}

	r, err := exampleMembership(t, "0").Resolve(records, now)
	require.NoError(t, err)
	assert.Equal(t, map[string]MemberState{"hub-a": StateActive, "hub-b": StateActive, "hub-c": StateWatching, "hub-d": StateActive}, r.States)
	assert.Equal(t, []string{"hub-a", "hub-b", "hub-d"}, r.Active, "watching hubs don't vote; non-members aren't listed")
	assert.Equal(t, 2, r.Quorum, "3 active: majority")

	r, err = exampleMembership(t, "7d").Resolve(records, now)
	require.NoError(t, err)
	assert.Equal(t, []string{"hub-a", "hub-d"}, r.Active, "hub-b forgotten after 7 days of silence")
	assert.Equal(t, 2, r.Quorum, "2 active: unanimous")
	assert.NotContains(t, r.States, "hub-b")
}

func TestResolveConflictIsAnError(t *testing.T) {
	m, err := ParseMembership(membershipObj(t, `
  members:
    - selector: {matchLabels: {multihub.argoproj.io/role: hub}}
      state: active
    - selector: {matchLabels: {region: eu-west}}
      state: watching
`))
	require.NoError(t, err)
	_, err = m.Resolve([]HubRecord{{ID: "hub-a", Labels: map[string]string{"multihub.argoproj.io/role": "hub", "region": "eu-west"}, LastSeen: time.Now()}}, time.Now())
	assert.Error(t, err, "the caller keeps its last valid membership")
}
