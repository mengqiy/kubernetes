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

package validation

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/apis/apiserver"
)

const (
	storageCompressionNilErr    = "StorageCompressionConfiguration can't be nil"
	unsupportedAlgorithmErrFmt  = "unsupported algorithm %q, supported values are %v"
	starCompressionResourceErr  = "use '*.' to select all resources from the core API group, or '*.*' to select all resources"
	compressionResourceNameErr  = "resource name should not contain capital letters"
	compressionNonRESTErr       = "resources which do not have REST API/s cannot be compressed"
	compressionExtensionsErr    = "'extensions' group has been removed and cannot be used for compression"
	compressionDuplicateSameFmt = "the same resource cannot be specified multiple times in one entry"
)

var supportedCompressionAlgorithms = []apiserver.CompressionAlgorithm{
	apiserver.CompressionAlgorithmDeflate,
	apiserver.CompressionAlgorithmNone,
}

// ValidateStorageCompressionConfiguration validates a StorageCompressionConfiguration.
//
// Note what is deliberately NOT validated: overlap of resource selections ACROSS
// entries. In this configuration the resource lists are ordered and the first
// match wins, so an earlier entry deliberately shadowing part of a later wildcard
// is the supported way to exclude a resource. That is a feature here, unlike in
// EncryptionConfiguration where cross-entry overlap is meaningless. Detecting an
// entry that is *entirely* shadowed by an earlier one requires the same matching
// semantics the storage layer applies, so it is checked by the caller rather than
// here, keeping this function a pure check on the shape of the configuration.
func ValidateStorageCompressionConfiguration(c *apiserver.StorageCompressionConfiguration) field.ErrorList {
	root := field.NewPath("resources")
	allErrs := field.ErrorList{}

	if c == nil {
		allErrs = append(allErrs, field.Required(root, storageCompressionNilErr))
		return allErrs
	}

	if len(c.Resources) == 0 {
		allErrs = append(allErrs, field.Required(root, fmt.Sprintf(atLeastOneRequiredErrFmt, root)))
		return allErrs
	}

	for i, entry := range c.Resources {
		r := root.Index(i).Child("resources")
		a := root.Index(i).Child("algorithm")

		if len(entry.Resources) == 0 {
			allErrs = append(allErrs, field.Required(r, fmt.Sprintf(atLeastOneRequiredErrFmt, r)))
		}

		// Overlap WITHIN a single entry is still meaningless: every resource in an
		// entry gets the same algorithm, so listing both 'secrets' and '*.' in one
		// entry says nothing that '*.' alone does not.
		allErrs = append(allErrs, validateResourceOverlap(entry.Resources, r)...)
		allErrs = append(allErrs, validateCompressionResourceNames(entry.Resources, r)...)

		if !isSupportedCompressionAlgorithm(entry.Algorithm) {
			allErrs = append(allErrs, field.Invalid(a, entry.Algorithm,
				fmt.Sprintf(unsupportedAlgorithmErrFmt, entry.Algorithm, supportedCompressionAlgorithms)))
		}
	}

	return allErrs
}

func isSupportedCompressionAlgorithm(a apiserver.CompressionAlgorithm) bool {
	for _, s := range supportedCompressionAlgorithms {
		if a == s {
			return true
		}
	}
	return false
}

// validateCompressionResourceNames checks the shape of resource selectors.
//
// This intentionally does not reuse validateResourceNames from the encryption
// validator, which additionally rejects the 'events.k8s.io' group because events
// are stored under the 'events' group in etcd and encrypting them via the wrong
// name would silently do nothing. For compression the same name is legitimate and
// useful: naming 'events.events.k8s.io' with algorithm None is how an operator
// excludes events from a wildcard, and rejecting it would remove the ability to
// exclude the resource this feature most wants excluded.
func validateCompressionResourceNames(resources []string, fieldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	seen := sets.New[string]()

	for j, res := range resources {
		jj := fieldPath.Index(j)

		if hasCapital(res) {
			allErrs = append(allErrs, field.Invalid(jj, res, compressionResourceNameErr))
			continue
		}

		if res == "*" {
			allErrs = append(allErrs, field.Invalid(jj, res, starCompressionResourceErr))
			continue
		}

		if res == "apiserveripinfo" || res == "serviceipallocations" || res == "servicenodeportallocations" {
			allErrs = append(allErrs, field.Invalid(jj, res, compressionNonRESTErr))
			continue
		}

		gr := schema.ParseGroupResource(res)
		if gr.Group == "extensions" {
			allErrs = append(allErrs, field.Invalid(jj, res, compressionExtensionsErr))
			continue
		}
		if len(gr.Resource) == 0 {
			allErrs = append(allErrs, field.Invalid(jj, res, "resource name must not be empty"))
			continue
		}

		if seen.Has(res) {
			allErrs = append(allErrs, field.Invalid(jj, res, compressionDuplicateSameFmt))
			continue
		}
		seen.Insert(res)
	}

	return allErrs
}
