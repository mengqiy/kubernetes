/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package compression

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/apis/apiserver"
)

func gr(group, resource string) schema.GroupResource {
	return schema.GroupResource{Group: group, Resource: resource}
}

// entry is a terse constructor for a config entry.
func entry(algorithm apiserver.CompressionAlgorithm, resources ...string) apiserver.StorageCompressionResourceConfiguration {
	return apiserver.StorageCompressionResourceConfiguration{Resources: resources, Algorithm: algorithm}
}

func config(entries ...apiserver.StorageCompressionResourceConfiguration) *apiserver.StorageCompressionConfiguration {
	return &apiserver.StorageCompressionConfiguration{Resources: entries}
}

const (
	deflate = apiserver.CompressionAlgorithmDeflate
	none    = apiserver.CompressionAlgorithmNone
)

func TestPolicyEnabledFor(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *apiserver.StorageCompressionConfiguration
		enabled  []schema.GroupResource
		disabled []schema.GroupResource
	}{
		{
			name:     "nil config selects nothing",
			cfg:      nil,
			disabled: []schema.GroupResource{gr("", "configmaps"), gr("", "secrets"), gr("example.com", "widgets")},
		},
		{
			name:     "empty config selects nothing",
			cfg:      config(),
			disabled: []schema.GroupResource{gr("", "configmaps")},
		},
		{
			name:     "bare resource is core group",
			cfg:      config(entry(deflate, "configmaps")),
			enabled:  []schema.GroupResource{gr("", "configmaps")},
			disabled: []schema.GroupResource{gr("apps", "configmaps"), gr("", "pods")},
		},
		{
			name:     "resource dot group",
			cfg:      config(entry(deflate, "widgets.example.com")),
			enabled:  []schema.GroupResource{gr("example.com", "widgets")},
			disabled: []schema.GroupResource{gr("", "widgets"), gr("example.com", "gadgets")},
		},
		{
			name:     "group wildcard",
			cfg:      config(entry(deflate, "*.example.com")),
			enabled:  []schema.GroupResource{gr("example.com", "widgets"), gr("example.com", "gadgets")},
			disabled: []schema.GroupResource{gr("", "configmaps"), gr("other.com", "widgets")},
		},
		{
			name:     "core group wildcard",
			cfg:      config(entry(deflate, "*.")),
			enabled:  []schema.GroupResource{gr("", "configmaps"), gr("", "pods")},
			disabled: []schema.GroupResource{gr("apps", "deployments")},
		},
		{
			name:    "everything wildcard",
			cfg:     config(entry(deflate, "*.*")),
			enabled: []schema.GroupResource{gr("", "configmaps"), gr("apps", "deployments"), gr("example.com", "widgets")},
			// Never by wildcard.
			disabled: []schema.GroupResource{gr("", "secrets")},
		},
		{
			name:     "algorithm None selects nothing",
			cfg:      config(entry(none, "*.*")),
			disabled: []schema.GroupResource{gr("", "configmaps"), gr("apps", "deployments")},
		},

		// Ordering: the first match wins. This is the exclusion mechanism.
		{
			name: "earlier None excludes a resource from a later wildcard",
			cfg: config(
				entry(none, "events", "events.events.k8s.io"),
				entry(deflate, "*.*"),
			),
			enabled:  []schema.GroupResource{gr("", "configmaps"), gr("apps", "deployments")},
			disabled: []schema.GroupResource{gr("", "events"), gr("events.k8s.io", "events")},
		},
		{
			name: "earlier None excludes a whole group from a later wildcard",
			cfg: config(
				entry(none, "*.coordination.k8s.io"),
				entry(deflate, "*.*"),
			),
			enabled:  []schema.GroupResource{gr("", "configmaps")},
			disabled: []schema.GroupResource{gr("coordination.k8s.io", "leases")},
		},
		{
			name: "later entry cannot override an earlier match",
			cfg: config(
				entry(deflate, "*.*"),
				entry(none, "configmaps"),
			),
			// configmaps was already claimed by the wildcard.
			enabled: []schema.GroupResource{gr("", "configmaps")},
		},
		{
			name: "an earlier Deflate wins over a later None",
			cfg: config(
				entry(deflate, "configmaps"),
				entry(none, "*."),
			),
			enabled:  []schema.GroupResource{gr("", "configmaps")},
			disabled: []schema.GroupResource{gr("", "pods")},
		},

		// Secrets.
		{
			name:     "secrets named explicitly is enabled",
			cfg:      config(entry(deflate, "secrets")),
			enabled:  []schema.GroupResource{gr("", "secrets")},
			disabled: []schema.GroupResource{gr("", "configmaps")},
		},
		{
			name:     "secrets is not reached by the everything wildcard",
			cfg:      config(entry(deflate, "*.*")),
			disabled: []schema.GroupResource{gr("", "secrets")},
		},
		{
			name:     "secrets is not reached by the core group wildcard",
			cfg:      config(entry(deflate, "*.")),
			disabled: []schema.GroupResource{gr("", "secrets")},
		},
		{
			name: "secrets can be enabled alongside a wildcard by naming it",
			cfg: config(
				entry(deflate, "secrets"),
				entry(deflate, "*.*"),
			),
			enabled: []schema.GroupResource{gr("", "secrets"), gr("", "configmaps")},
		},
		{
			name: "naming secrets after a wildcard still works, because no wildcard claims it",
			cfg: config(
				entry(deflate, "*.*"),
				entry(deflate, "secrets"),
			),
			enabled: []schema.GroupResource{gr("", "secrets"), gr("", "configmaps")},
		},
		{
			name:     "a secrets resource in another group is not the sensitive one",
			cfg:      config(entry(deflate, "*.example.com")),
			enabled:  []schema.GroupResource{gr("example.com", "secrets")},
			disabled: []schema.GroupResource{gr("", "secrets")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPolicy(tc.cfg)
			if err != nil {
				t.Fatalf("NewPolicy: %v", err)
			}
			for _, r := range tc.enabled {
				if !p.EnabledFor(r) {
					t.Errorf("EnabledFor(%s) = false, want true", r)
				}
			}
			for _, r := range tc.disabled {
				if p.EnabledFor(r) {
					t.Errorf("EnabledFor(%s) = true, want false", r)
				}
			}
		})
	}
}

func TestPolicyAlgorithmDefaultsToDeflate(t *testing.T) {
	// Defaulting normally supplies the algorithm, but a config built in Go without
	// running the defaulters must not silently mean "no compression".
	p, err := NewPolicy(config(apiserver.StorageCompressionResourceConfiguration{Resources: []string{"configmaps"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.AlgorithmFor(gr("", "configmaps")); got != deflate {
		t.Fatalf("AlgorithmFor = %q, want %q", got, deflate)
	}
}

func TestPolicyAlgorithmForUnmatched(t *testing.T) {
	p, err := NewPolicy(config(entry(deflate, "configmaps")))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.AlgorithmFor(gr("", "pods")); got != none {
		t.Fatalf("AlgorithmFor(pods) = %q, want %q", got, none)
	}
}

func TestNilPolicyIsInert(t *testing.T) {
	var p *Policy
	if p.EnabledFor(gr("", "configmaps")) {
		t.Error("a nil policy must select nothing")
	}
	if !p.IsEmpty() {
		t.Error("a nil policy must report IsEmpty")
	}
	if got := p.AlgorithmFor(gr("", "configmaps")); got != none {
		t.Errorf("AlgorithmFor on a nil policy = %q, want %q", got, none)
	}
	if s := p.ShadowedSelectors(nil); len(s) != 0 {
		t.Errorf("ShadowedSelectors on a nil policy = %v, want none", s)
	}
}

func TestPolicyIsEmpty(t *testing.T) {
	cases := []struct {
		cfg  *apiserver.StorageCompressionConfiguration
		want bool
	}{
		{nil, true},
		{config(), true},
		{config(entry(deflate, "configmaps")), false},
		// An all-None config still has entries; it is not "empty", it is
		// deliberately selecting nothing. IsEmpty is about shape, not effect.
		{config(entry(none, "*.*")), false},
	}
	for _, tc := range cases {
		p, err := NewPolicy(tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.IsEmpty(); got != tc.want {
			t.Errorf("IsEmpty() = %v, want %v for %+v", got, tc.want, tc.cfg)
		}
	}
}

func TestNewPolicyRejectsMalformedSelectors(t *testing.T) {
	if _, err := NewPolicy(config(entry(deflate, ""))); err == nil {
		t.Fatal("expected an error for an empty resource selector")
	}
}

// TestShadowedSelectors covers the dead-configuration detection. Ordering is the
// exclusion mechanism, so overlap is legal -- but a selector an earlier entry
// already claims can never take effect, and is almost always a misordered file.
func TestShadowedSelectors(t *testing.T) {
	cases := []struct {
		name string
		cfg  *apiserver.StorageCompressionConfiguration
		want []ShadowedSelector
	}{
		{
			name: "no shadowing",
			cfg: config(
				entry(none, "events"),
				entry(deflate, "*.*"),
			),
		},
		{
			name: "exact resource after everything wildcard is dead",
			cfg: config(
				entry(deflate, "*.*"),
				entry(none, "configmaps"),
			),
			want: []ShadowedSelector{{Entry: 1, Selector: "configmaps", ShadowedBy: 0}},
		},
		{
			name: "group wildcard after everything wildcard is dead",
			cfg: config(
				entry(deflate, "*.*"),
				entry(none, "*.coordination.k8s.io"),
			),
			want: []ShadowedSelector{{Entry: 1, Selector: "*.coordination.k8s.io", ShadowedBy: 0}},
		},
		{
			name: "exact resource after its own group wildcard is dead",
			cfg: config(
				entry(deflate, "*.example.com"),
				entry(none, "widgets.example.com"),
			),
			want: []ShadowedSelector{{Entry: 1, Selector: "widgets.example.com", ShadowedBy: 0}},
		},
		{
			name: "repeating an exact resource is dead",
			cfg: config(
				entry(deflate, "configmaps"),
				entry(none, "configmaps"),
			),
			want: []ShadowedSelector{{Entry: 1, Selector: "configmaps", ShadowedBy: 0}},
		},
		{
			name: "secrets after a wildcard is NOT shadowed, because no wildcard claims it",
			cfg: config(
				entry(deflate, "*.*"),
				entry(deflate, "secrets"),
			),
		},
		{
			name: "a different group is not shadowed",
			cfg: config(
				entry(deflate, "*.example.com"),
				entry(none, "*.other.com"),
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPolicy(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := p.ShadowedSelectors(tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d shadowed selectors %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
			// The message must name the offending entry so an operator can fix it.
			for _, s := range got {
				if msg := s.String(); len(msg) == 0 {
					t.Error("ShadowedSelector.String() must not be empty")
				}
			}
		})
	}
}

func TestSplitResource(t *testing.T) {
	cases := []struct {
		in       string
		resource string
		group    string
		ok       bool
	}{
		{"configmaps", "configmaps", "", true},
		{"widgets.example.com", "widgets", "example.com", true},
		{"*.", "*", "", true},
		{"*.*", "*", "*", true},
		{"*.example.com", "*", "example.com", true},
		{"", "", "", false},
	}
	for _, tc := range cases {
		resource, group, ok := splitResource(tc.in)
		if resource != tc.resource || group != tc.group || ok != tc.ok {
			t.Errorf("splitResource(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, resource, group, ok, tc.resource, tc.group, tc.ok)
		}
	}
}
