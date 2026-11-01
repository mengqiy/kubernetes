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
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/internal/storagecompression"
)

// minMinSize is 64 because DEFLATE almost never shrinks a smaller value, which
// then costs a 9-byte verbatim frame and the CPU spent trying.
const minMinSize = 64

var supportedCompressionAlgorithms = []CompressionAlgorithm{
	CompressionAlgorithmDeflate,
	CompressionAlgorithmNone,
}

// ValidateStorageCompressionConfiguration checks each entry's resources,
// algorithm and minSize, then rejects any selector that an earlier entry
// repeats or masks.
func ValidateStorageCompressionConfiguration(c *StorageCompressionConfiguration) field.ErrorList {
	root := field.NewPath("resources")
	if c == nil {
		return field.ErrorList{field.Required(root, "StorageCompressionConfiguration can't be nil")}
	}

	var allErrs field.ErrorList
	entries := make([][]string, 0, len(c.Resources))
	for i, entry := range c.Resources {
		p := root.Index(i)
		if len(entry.Resources) == 0 {
			allErrs = append(allErrs, field.Required(p.Child("resources"), ""))
		}
		allErrs = append(allErrs, storagecompression.ValidateSelectors(entry.Resources, p.Child("resources"))...)
		allErrs = append(allErrs, validateCompressionAlgorithm(entry.Algorithm, p.Child("algorithm"))...)
		allErrs = append(allErrs, validateCompressionMinSize(entry.MinSize, p.Child("minSize"))...)
		entries = append(entries, entry.Resources)
	}
	return append(allErrs, storagecompression.ValidateMasking(entries, root)...)
}

func validateCompressionAlgorithm(algorithm CompressionAlgorithm, fldPath *field.Path) field.ErrorList {
	switch algorithm {
	case CompressionAlgorithmDeflate, CompressionAlgorithmNone:
		return nil
	case "":
		return field.ErrorList{field.Required(fldPath, "")}
	default:
		return field.ErrorList{field.NotSupported(fldPath, algorithm, supportedCompressionAlgorithms)}
	}
}

// validateCompressionMinSize checks the floor before the ceiling and the
// whole-bytes rule, so a value below 64 gets one error.
func validateCompressionMinSize(minSize *resource.Quantity, fldPath *field.Path) field.ErrorList {
	if minSize == nil {
		return nil
	}
	floor := resource.NewQuantity(minMinSize, resource.BinarySI)
	if minSize.Cmp(*floor) < 0 {
		return field.ErrorList{field.Invalid(fldPath, minSize.String(), fmt.Sprintf("must be at least %s", floor)).WithOrigin("minimum")}
	}
	return storagecompression.ValidateMinSize(minSize, fldPath)
}
