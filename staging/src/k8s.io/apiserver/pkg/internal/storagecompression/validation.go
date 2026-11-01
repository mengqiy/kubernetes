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

package storagecompression

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var anyGroupAnyResource = schema.GroupResource{Group: "*", Resource: "*"}

// ValidateSelectors checks one entry's resources. Each must be a resource or a
// wildcard that EncryptionConfiguration would also accept, and none may repeat
// or overlap another. A problem is reported once, on the later selector.
func ValidateSelectors(resources []string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	earlier := make([]schema.GroupResource, 0, len(resources))
	for j, res := range resources {
		if err := selectorNameError(res, fldPath.Index(j)); err != nil {
			allErrs = append(allErrs, err)
			continue
		}
		gr := schema.ParseGroupResource(res)
		switch {
		case slices.Contains(earlier, gr):
			allErrs = append(allErrs, field.Duplicate(fldPath.Index(j), res))
		case slices.ContainsFunc(earlier, func(e schema.GroupResource) bool { return overlap(e, gr) }):
			allErrs = append(allErrs, field.Invalid(fldPath.Index(j), res,
				"using overlapping resources such as 'configmaps' and '*.' in the same resource list is not allowed as they will be masked").WithOrigin("overlap"))
		}
		earlier = append(earlier, gr)
	}
	return allErrs
}

// selectorNameError copies EncryptionConfiguration's resource name rules, and
// TestSelectorsAgreeWithEncryption fails if the two drift apart.
func selectorNameError(res string, p *field.Path) *field.Error {
	gr := schema.ParseGroupResource(res)
	switch {
	case strings.ToLower(res) != res:
		return field.Invalid(p, res, "resource name should not contain capital letters").WithOrigin("lowercase")
	case res == "*":
		return field.Invalid(p, res, "use '*.' to compress the core API group or '*.*' to compress every group; neither selects core secrets, which have to be named").WithOrigin("bareWildcard")
	case res == "apiserveripinfo" || res == "serviceipallocations" || res == "servicenodeportallocations":
		return field.Invalid(p, res, "resources which do not have REST API/s cannot be compressed").WithOrigin("nonRESTResource")
	case gr.Group == "events.k8s.io":
		return field.Invalid(p, res, "'*.events.k8s.io' objects are stored using the 'events' API group in etcd. Use 'events' instead in the config file").WithOrigin("eventsGroup")
	case gr.Group == "extensions":
		return field.Invalid(p, res, "'extensions' group has been removed and cannot be used for compression").WithOrigin("extensionsGroup")
	case gr.Group == "*" && gr.Resource != "*":
		return field.Invalid(p, res, "compressing the same resource across groups is not supported").WithOrigin("acrossGroups")
	}
	return nil
}

// overlap reports whether two different selectors can select the same resource.
// Core secrets overlap nothing, because no wildcard selects them.
func overlap(a, b schema.GroupResource) bool {
	if a == SecretsGroupResource() || b == SecretsGroupResource() {
		return false
	}
	if a == anyGroupAnyResource || b == anyGroupAnyResource {
		return true
	}
	return a.Group == b.Group && (a.Resource == "*" || b.Resource == "*")
}

// ValidateMasking rejects a selector that an earlier entry repeats or covers,
// since the first match wins and it could never take effect. ValidateSelectors
// covers each entry on its own, so selectors it rejects are skipped here and no
// mistake is reported twice.
func ValidateMasking(entries [][]string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	// decidedBy maps each selector to the path that decides it under first-match:
	// its first occurrence or, for a masked selector, the root of the chain that
	// masks it. Errors name that path.
	decidedBy := map[schema.GroupResource]*field.Path{}
	for i, resources := range entries {
		entryPath := fldPath.Index(i).Child("resources")
		flagged := map[string]bool{}
		for _, err := range ValidateSelectors(resources, entryPath) {
			flagged[err.Field] = true
		}
		inEntry := map[schema.GroupResource]*field.Path{}
		for j, res := range resources {
			p := entryPath.Index(j)
			if flagged[p.String()] {
				continue
			}
			gr := schema.ParseGroupResource(res)
			inEntry[gr] = p
			for _, selector := range CoveringSelectors(gr) {
				by, ok := decidedBy[selector]
				if !ok {
					continue
				}
				if selector == gr {
					allErrs = append(allErrs, field.Duplicate(p, res))
				} else {
					allErrs = append(allErrs, field.Invalid(p, res, fmt.Sprintf("masked by %s", by)).WithOrigin("masked"))
				}
				inEntry[gr] = by
				break
			}
		}
		maps.Copy(decidedBy, inEntry)
	}
	return allErrs
}

// ValidateMinSize keeps minSize below MaxPlaintextBytes and a whole number of
// bytes. RoundUp spots a fraction at any magnitude, while Value saturates
// beyond int64 and would make a huge negative number look fractional.
func ValidateMinSize(minSize *resource.Quantity, fldPath *field.Path) field.ErrorList {
	if minSize == nil {
		return nil
	}
	ceiling := resource.NewQuantity(MaxPlaintextBytes, resource.BinarySI)
	if minSize.Cmp(*ceiling) >= 0 {
		return field.ErrorList{field.Invalid(fldPath, minSize.String(), fmt.Sprintf("must be less than %s", ceiling)).WithOrigin("maximum")}
	}
	if whole := *minSize; !whole.RoundUp(0) {
		return field.ErrorList{field.Invalid(fldPath, minSize.String(), "must be a whole number of bytes").WithOrigin("wholeNumber")}
	}
	return nil
}
