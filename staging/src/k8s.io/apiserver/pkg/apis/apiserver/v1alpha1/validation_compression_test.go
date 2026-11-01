/*
Copyright The Kubernetes Authors.

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

package v1alpha1

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/operation"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func compressionConfig(entries ...CompressionResourceConfiguration) *StorageCompressionConfiguration {
	return &StorageCompressionConfiguration{Resources: entries}
}

func deflateEntry(resources ...string) CompressionResourceConfiguration {
	return CompressionResourceConfiguration{Resources: resources, Algorithm: CompressionAlgorithmDeflate}
}

func minSizeEntry(resource, minSize string) CompressionResourceConfiguration {
	e := deflateEntry(resource)
	e.MinSize = new(apiresource.MustParse(minSize))
	return e
}

// TestValidateStorageCompressionConfiguration covers the generated rules and the
// custom ones together, matching errors by type, field and origin. A masking
// error's detail must name the selector that masks it. The floor cases pin
// +k8s:minimum=64: DEFLATE almost never shrinks a smaller value, which then
// costs a 9-byte verbatim frame and the CPU spent trying.
func TestValidateStorageCompressionConfiguration(t *testing.T) {
	root := field.NewPath("resources")
	resources := root.Index(0).Child("resources")
	selector := func(i, j int) *field.Path { return root.Index(i).Child("resources").Index(j) }
	minSize := func(i int) *field.Path { return root.Index(i).Child("minSize") }
	matcher := field.ErrorMatcher{}.ByType().ByField().ByOrigin().ByDetailSubstring().RequireOriginWhenInvalid()

	for _, tc := range []struct {
		desc string
		in   *StorageCompressionConfiguration
		want field.ErrorList
	}{{
		desc: "no entries compress nothing",
		in:   compressionConfig(),
	}, {
		desc: "an entry with no resources",
		in:   compressionConfig(deflateEntry()),
		want: field.ErrorList{field.Required(resources, "")},
	}, {
		desc: "every selector form",
		in: compressionConfig(
			CompressionResourceConfiguration{Resources: []string{"events", "*.coordination.k8s.io"}, Algorithm: CompressionAlgorithmNone},
			CompressionResourceConfiguration{Resources: []string{"widgets.example.com"}, Algorithm: CompressionAlgorithmDeflate, MinSize: new(apiresource.MustParse("4Ki"))},
			deflateEntry("*.apps", "secrets"),
			deflateEntry("*."),
			deflateEntry("*.*"),
		),
	}, {
		desc: "selectors that can't match a stored resource",
		in: compressionConfig(
			deflateEntry("ConfigMaps"), deflateEntry("*"), deflateEntry("serviceipallocations"),
			deflateEntry("events.events.k8s.io"), deflateEntry("*.events.k8s.io"),
			deflateEntry("deployments.extensions"), deflateEntry("configmaps.*"),
		),
		want: field.ErrorList{
			field.Invalid(selector(0, 0), nil, "").WithOrigin("lowercase"),
			field.Invalid(selector(1, 0), nil, "").WithOrigin("bareWildcard"),
			field.Invalid(selector(2, 0), nil, "").WithOrigin("nonRESTResource"),
			field.Invalid(selector(3, 0), nil, "").WithOrigin("eventsGroup"),
			field.Invalid(selector(4, 0), nil, "").WithOrigin("eventsGroup"),
			field.Invalid(selector(5, 0), nil, "").WithOrigin("extensionsGroup"),
			field.Invalid(selector(6, 0), nil, "").WithOrigin("acrossGroups"),
		},
	}, {
		desc: "a resource beside its group wildcard",
		in:   compressionConfig(deflateEntry("configmaps", "*.")),
		want: field.ErrorList{field.Invalid(selector(0, 1), nil, "").WithOrigin("overlap")},
	}, {
		desc: "a group wildcard beside the everything wildcard",
		in:   compressionConfig(deflateEntry("*.*", "*.apps")),
		want: field.ErrorList{field.Invalid(selector(0, 1), nil, "").WithOrigin("overlap")},
	}, {
		desc: "each overlap reported once, on the later selector",
		in:   compressionConfig(deflateEntry("*.apps", "*.", "deployments.apps", "*.*")),
		want: field.ErrorList{
			field.Invalid(selector(0, 2), nil, "").WithOrigin("overlap"),
			field.Invalid(selector(0, 3), nil, "").WithOrigin("overlap"),
		},
	}, {
		desc: "an overlap with a selector other than the first",
		in:   compressionConfig(deflateEntry("configmaps", "deployments.apps", "*.apps")),
		want: field.ErrorList{field.Invalid(selector(0, 2), nil, "").WithOrigin("overlap")},
	}, {
		desc: "a rejected selector isn't also an overlap or a duplicate",
		in:   compressionConfig(deflateEntry("configmaps", "*"), deflateEntry("*.*", "ConfigMaps"), deflateEntry("*", "*.")),
		want: field.ErrorList{
			field.Invalid(selector(0, 1), nil, "").WithOrigin("bareWildcard"),
			field.Invalid(selector(1, 1), nil, "").WithOrigin("lowercase"),
			field.Invalid(selector(2, 0), nil, "").WithOrigin("bareWildcard"),
			field.Invalid(selector(2, 1), nil, "masked by resources[1].resources[0]").WithOrigin("masked"),
		},
	}, {
		desc: "secrets beside wildcards, which never select it",
		in:   compressionConfig(deflateEntry("secrets", "*.", "*.apps")),
	}, {
		desc: "secrets beside the everything wildcard",
		in:   compressionConfig(deflateEntry("secrets", "*.*")),
	}, {
		desc: "secrets after the everything wildcard",
		in:   compressionConfig(deflateEntry("*.*", "secrets")),
	}, {
		desc: "secrets after group wildcards",
		in:   compressionConfig(deflateEntry("*.", "*.apps", "secrets")),
	}, {
		desc: "every repeat of a resource",
		in:   compressionConfig(deflateEntry("secrets", "secrets.", "secrets")),
		want: field.ErrorList{field.Duplicate(selector(0, 1), nil), field.Duplicate(selector(0, 2), nil)},
	}, {
		desc: "a repeated wildcard is only a duplicate",
		in:   compressionConfig(deflateEntry("*.apps", "*.apps")),
		want: field.ErrorList{field.Duplicate(selector(0, 1), nil)},
	}, {
		desc: "a missing algorithm",
		in:   compressionConfig(CompressionResourceConfiguration{Resources: []string{"configmaps"}}),
		want: field.ErrorList{field.Required(root.Index(0).Child("algorithm"), "")},
	}, {
		desc: "an unknown algorithm, including a known one in another case",
		in: compressionConfig(
			CompressionResourceConfiguration{Resources: []string{"configmaps"}, Algorithm: "Zstd"},
			CompressionResourceConfiguration{Resources: []string{"pods"}, Algorithm: "deflate"},
		),
		want: field.ErrorList{
			field.NotSupported[CompressionAlgorithm](root.Index(0).Child("algorithm"), nil, nil),
			field.NotSupported[CompressionAlgorithm](root.Index(1).Child("algorithm"), nil, nil),
		},
	}, {
		desc: "minSize at both ends of its range",
		in: compressionConfig(
			minSizeEntry("configmaps", "64"),
			minSizeEntry("secrets", "64.0"),
			minSizeEntry("pods", "1471Ki"),
			minSizeEntry("services", "1507327"),
		),
	}, {
		desc: "minSize outside its range or not a whole number of bytes",
		in: compressionConfig(
			minSizeEntry("configmaps", "63"),
			minSizeEntry("pods", "1472Ki"),
			minSizeEntry("services", "512m"),
			minSizeEntry("nodes", "-1"),
			minSizeEntry("leases.coordination.k8s.io", "1e30"),
			minSizeEntry("widgets.example.com", "100.5"),
			minSizeEntry("gadgets.example.com", "0.1Ki"),
			minSizeEntry("endpoints", "63.999"),
			minSizeEntry("events", "-1e30"),
		),
		want: field.ErrorList{
			field.Invalid(minSize(0), nil, "").WithOrigin("minimum"),
			field.Invalid(minSize(1), nil, "").WithOrigin("maximum"),
			// 512m is below the floor and fractional, and each rule reports it.
			field.Invalid(minSize(2), nil, "").WithOrigin("minimum"),
			field.Invalid(minSize(2), nil, "").WithOrigin("wholeNumber"),
			field.Invalid(minSize(3), nil, "").WithOrigin("minimum"),
			field.Invalid(minSize(4), nil, "").WithOrigin("maximum"),
			field.Invalid(minSize(5), nil, "").WithOrigin("wholeNumber"),
			field.Invalid(minSize(6), nil, "").WithOrigin("wholeNumber"),
			field.Invalid(minSize(7), nil, "").WithOrigin("minimum"),
			field.Invalid(minSize(7), nil, "").WithOrigin("wholeNumber"),
			field.Invalid(minSize(8), nil, "").WithOrigin("minimum"),
		},
	}, {
		desc: "every selector an earlier entry masks, reported together",
		in: compressionConfig(
			deflateEntry("pods", "*.example.com"),
			deflateEntry("*.*"),
			deflateEntry("configmaps", "*.coordination.k8s.io"),
			deflateEntry("*."),
			deflateEntry("widgets.example.com", "pods"),
			deflateEntry("*.example.com"),
			deflateEntry("gadgets.example.com"),
		),
		want: field.ErrorList{
			field.Invalid(selector(2, 0), nil, "masked by resources[1].resources[0]").WithOrigin("masked"),
			field.Invalid(selector(2, 1), nil, "masked by resources[1].resources[0]").WithOrigin("masked"),
			field.Invalid(selector(3, 0), nil, "masked by resources[1].resources[0]").WithOrigin("masked"),
			field.Invalid(selector(4, 0), nil, "masked by resources[0].resources[1]").WithOrigin("masked"),
			field.Duplicate(selector(4, 1), nil),
			field.Duplicate(selector(5, 0), nil),
			// Of two identical selectors, the first one masks.
			field.Invalid(selector(6, 0), nil, "masked by resources[0].resources[1]").WithOrigin("masked"),
		},
	}, {
		desc: "a chain of masks names the selector at its root",
		in:   compressionConfig(deflateEntry("*.*"), deflateEntry("*.", "*.apps"), deflateEntry("configmaps", "deployments.apps")),
		want: field.ErrorList{
			field.Invalid(selector(1, 0), nil, "masked by resources[0].resources[0]").WithOrigin("masked"),
			field.Invalid(selector(1, 1), nil, "masked by resources[0].resources[0]").WithOrigin("masked"),
			field.Invalid(selector(2, 0), nil, "masked by resources[0].resources[0]").WithOrigin("masked"),
			field.Invalid(selector(2, 1), nil, "masked by resources[0].resources[0]").WithOrigin("masked"),
		},
	}, {
		desc: "a repeat within an entry is reported once when an earlier entry also has it",
		in:   compressionConfig(deflateEntry("configmaps"), deflateEntry("configmaps", "configmaps")),
		want: field.ErrorList{field.Duplicate(selector(1, 0), nil), field.Duplicate(selector(1, 1), nil)},
	}, {
		desc: "a repeat within an entry is reported once when an earlier wildcard masks it",
		in:   compressionConfig(deflateEntry("*.*"), deflateEntry("*.", "*.")),
		want: field.ErrorList{
			field.Invalid(selector(1, 0), nil, "masked by resources[0].resources[0]").WithOrigin("masked"),
			field.Duplicate(selector(1, 1), nil),
		},
	}, {
		desc: "an overlap within an entry isn't also a duplicate of an earlier entry",
		in:   compressionConfig(deflateEntry("*.apps"), deflateEntry("*.*", "*.apps")),
		want: field.ErrorList{field.Invalid(selector(1, 1), nil, "").WithOrigin("overlap")},
	}, {
		desc: "an overlap within an entry isn't also masked by an earlier entry",
		in:   compressionConfig(deflateEntry("*.*"), deflateEntry("*.", "configmaps")),
		want: field.ErrorList{
			field.Invalid(selector(1, 0), nil, "masked by resources[0].resources[0]").WithOrigin("masked"),
			field.Invalid(selector(1, 1), nil, "").WithOrigin("overlap"),
		},
	}, {
		desc: "a rejected selector neither masks nor is masked",
		in:   compressionConfig(deflateEntry("*"), deflateEntry("configmaps"), deflateEntry("*.*"), deflateEntry("*")),
		want: field.ErrorList{
			field.Invalid(selector(0, 0), nil, "").WithOrigin("bareWildcard"),
			field.Invalid(selector(3, 0), nil, "").WithOrigin("bareWildcard"),
		},
	}, {
		desc: "earlier selectors that leave a later one reachable",
		in: compressionConfig(
			CompressionResourceConfiguration{Resources: []string{"events", "*.coordination.k8s.io"}, Algorithm: CompressionAlgorithmNone},
			deflateEntry("*.example.com"),
			deflateEntry("*.other.com"),
			deflateEntry("*."),
			deflateEntry("*.*"),
			deflateEntry("secrets"),
		),
	}} {
		t.Run(tc.desc, func(t *testing.T) {
			matcher.Test(t, tc.want, Validate_StorageCompressionConfiguration(context.Background(), operation.Operation{Type: operation.Create}, nil, tc.in, nil))
		})
	}
}
