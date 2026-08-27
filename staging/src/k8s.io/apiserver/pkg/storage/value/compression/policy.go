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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/apis/apiserver"
)

// SecretsGroupResource is never selected by a wildcard. Compressing a Secret
// before encrypting it reveals the compressibility of its plaintext to anyone
// able to read etcd or an etcd backup, which is precisely the adversary that
// encryption at rest defends against. Enabling it therefore requires naming the
// resource explicitly, which is a deliberate enough act to serve as the
// acknowledgement.
var SecretsGroupResource = schema.GroupResource{Group: "", Resource: "secrets"}

// Policy decides, per resource, which compression algorithm the write path
// applies. It never affects the read path: reads are format-driven and
// unconditional, so a value framed by any apiserver is readable by every
// apiserver of the same or a newer version whatever its configuration.
//
// Entries are evaluated in order and the FIRST entry matching a resource wins.
// That is what makes exclusion expressible: an earlier entry with algorithm None
// removes a resource from a wildcard in a later entry.
//
// A nil *Policy, and one built from a nil configuration, select nothing. Default
// inertness is an enforced invariant rather than a convention: with no policy the
// transformer is a no-op in both directions, so every existing test that reads raw
// etcd bytes and assumes they are an untouched serialized object keeps passing.
type Policy struct {
	entries []policyEntry
}

type policyEntry struct {
	// exact holds resources named without a wildcard.
	exact sets.Set[schema.GroupResource]
	// groups holds the groups selected by "*.<group>", including "" for "*.".
	groups sets.Set[string]
	// all is set by "*.*".
	all bool

	algorithm apiserver.CompressionAlgorithm
}

// NewPolicy builds a Policy from a validated configuration. A nil configuration
// yields a nil Policy, which selects nothing.
//
// The configuration is expected to have been validated already, by
// validation.ValidateStorageCompressionConfiguration; NewPolicy still rejects
// malformed selectors rather than silently ignoring them, so that a caller which
// skipped validation fails loudly instead of quietly compressing the wrong set.
func NewPolicy(cfg *apiserver.StorageCompressionConfiguration) (*Policy, error) {
	if cfg == nil {
		return nil, nil
	}

	p := &Policy{}
	for i, entry := range cfg.Resources {
		pe := policyEntry{
			exact:     sets.New[schema.GroupResource](),
			groups:    sets.New[string](),
			algorithm: entry.Algorithm,
		}
		if len(pe.algorithm) == 0 {
			// Defaulting normally supplies this; be explicit rather than treating
			// an empty algorithm as "no compression" by accident.
			pe.algorithm = apiserver.CompressionAlgorithmDeflate
		}

		for _, raw := range entry.Resources {
			resource, group, found := splitResource(raw)
			if !found {
				return nil, fmt.Errorf("resources[%d].resources: invalid entry %q", i, raw)
			}
			switch {
			case resource == "*" && group == "*":
				pe.all = true
			case resource == "*":
				pe.groups.Insert(group)
			default:
				pe.exact.Insert(schema.GroupResource{Group: group, Resource: resource})
			}
		}
		p.entries = append(p.entries, pe)
	}
	return p, nil
}

// splitResource splits "<resource>" or "<resource>.<group>" into its parts.
// A bare name is in the core group, so "configmaps" is {configmaps, ""} and
// "*." is {*, ""}.
func splitResource(raw string) (resource, group string, ok bool) {
	if len(raw) == 0 {
		return "", "", false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] == '.' {
			return raw[:i], raw[i+1:], true
		}
	}
	return raw, "", true
}

// AlgorithmFor returns the algorithm the first matching entry selects, or None if
// no entry matches.
func (p *Policy) AlgorithmFor(gr schema.GroupResource) apiserver.CompressionAlgorithm {
	if p == nil {
		return apiserver.CompressionAlgorithmNone
	}
	for i := range p.entries {
		if p.entries[i].matches(gr) {
			return p.entries[i].algorithm
		}
	}
	return apiserver.CompressionAlgorithmNone
}

// EnabledFor reports whether the write path should frame values of gr.
func (p *Policy) EnabledFor(gr schema.GroupResource) bool {
	return p.AlgorithmFor(gr) == apiserver.CompressionAlgorithmDeflate
}

func (e *policyEntry) matches(gr schema.GroupResource) bool {
	// An explicit name always matches, including for secrets.
	if e.exact.Has(gr) {
		return true
	}
	// No wildcard ever reaches secrets.
	if gr == SecretsGroupResource {
		return false
	}
	if e.all {
		return true
	}
	return e.groups.Has(gr.Group)
}

// IsEmpty reports whether the policy has no entries at all, in which case the
// feature is entirely inert on the write path.
func (p *Policy) IsEmpty() bool {
	return p == nil || len(p.entries) == 0
}

// ShadowedSelector describes a resource selector that can never take effect
// because an earlier entry already claims it.
type ShadowedSelector struct {
	// Entry is the index of the entry containing the dead selector.
	Entry int
	// Selector is the resource selector as written.
	Selector string
	// ShadowedBy is the index of the earlier entry that already claims it.
	ShadowedBy int
}

func (s ShadowedSelector) String() string {
	return fmt.Sprintf("resources[%d].resources %q is already selected by resources[%d] and has no effect; "+
		"entries are evaluated in order and the first match wins, so move it earlier to take precedence",
		s.Entry, s.Selector, s.ShadowedBy)
}

// ShadowedSelectors reports selectors that cannot take effect because an earlier
// entry already claims them. Ordering is the exclusion mechanism, so overlap
// between entries is expected and legal -- but a selector that is *entirely*
// shadowed is dead configuration, and almost always a misordered file rather than
// an intent. Reported so a caller can surface it instead of silently doing
// nothing.
//
// Note that naming secrets after a wildcard is NOT shadowed, because no wildcard
// selects secrets.
func (p *Policy) ShadowedSelectors(cfg *apiserver.StorageCompressionConfiguration) []ShadowedSelector {
	if p == nil || cfg == nil {
		return nil
	}

	var out []ShadowedSelector
	for i, entry := range cfg.Resources {
		if i == 0 {
			continue
		}
		for _, raw := range entry.Resources {
			resource, group, ok := splitResource(raw)
			if !ok {
				continue
			}
			// Represent the selector as the GroupResource an earlier entry would
			// have to match. For a wildcard selector this is a synthetic
			// GroupResource whose Resource is "*", which the earlier entry's own
			// wildcards match exactly as they would any concrete resource.
			probe := schema.GroupResource{Group: group, Resource: resource}
			if resource == "*" && group == "*" {
				probe = schema.GroupResource{Group: "*", Resource: "*"}
			}
			for j := 0; j < i; j++ {
				if p.entries[j].matches(probe) {
					out = append(out, ShadowedSelector{Entry: i, Selector: raw, ShadowedBy: j})
					break
				}
			}
		}
	}
	return out
}
