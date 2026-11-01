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

package storagecompression_test

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/apis/apiserver/validation"
	"k8s.io/apiserver/pkg/internal/storagecompression"
)

// TestSelectorsAgreeWithEncryption runs every list of up to three of these
// selectors through both configurations' checks, which must agree on what's
// valid. The one intended difference is skipped: compression lets core secrets
// sit beside a core wildcard, because no wildcard selects them.
func TestSelectorsAgreeWithEncryption(t *testing.T) {
	selectors := []string{
		"configmaps", "configmaps.", "secrets", "secrets.", "pods", "deployments.apps",
		"leases.coordination.k8s.io", "widgets.example.com", "*.apps", "*.example.com", "*.", "*.*",
		"*", "ConfigMaps", "apiserveripinfo", "serviceipallocations", "servicenodeportallocations",
		"events", "events.events.k8s.io", "*.events.k8s.io", "deployments.extensions", "*.extensions", "configmaps.*",
	}
	coreWildcards := []schema.GroupResource{{Group: "", Resource: "*"}, {Group: "*", Resource: "*"}}

	var lists [][]string
	for _, a := range selectors {
		lists = append(lists, []string{a})
		for _, b := range selectors {
			lists = append(lists, []string{a, b})
			for _, c := range selectors {
				lists = append(lists, []string{a, b, c})
			}
		}
	}

	compared := 0
	for _, list := range lists {
		var hasSecrets, hasCoreWildcard bool
		for _, s := range list {
			gr := schema.ParseGroupResource(s)
			hasSecrets = hasSecrets || gr == storagecompression.SecretsGroupResource()
			hasCoreWildcard = hasCoreWildcard || slices.Contains(coreWildcards, gr)
		}
		if hasSecrets && hasCoreWildcard {
			continue
		}
		encryption := &apiserver.EncryptionConfiguration{Resources: []apiserver.ResourceConfiguration{{
			Resources: list,
			Providers: []apiserver.ProviderConfiguration{{Identity: &apiserver.IdentityConfiguration{}}},
		}}}
		encryptionOK := len(validation.ValidateEncryptionConfiguration(encryption, false)) == 0
		compressionOK := len(storagecompression.ValidateSelectors(list, field.NewPath("resources"))) == 0
		if encryptionOK != compressionOK {
			t.Errorf("%q: EncryptionConfiguration accepts it: %v, compression accepts it: %v", list, encryptionOK, compressionOK)
		}
		compared++
	}
	if compared < len(lists)/2 {
		t.Fatalf("compared only %d of %d lists", compared, len(lists))
	}
}
