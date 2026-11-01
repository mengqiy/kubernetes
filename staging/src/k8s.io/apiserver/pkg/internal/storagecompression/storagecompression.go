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

// Package storagecompression holds storage compression's validation and what it,
// the config loader and the transformer have to agree on. It's internal,
// importable only from k8s.io/apiserver/pkg/..., so none of it is public API.
// See KEP-6264.
package storagecompression

import "k8s.io/apimachinery/pkg/runtime/schema"

// MaxPlaintextBytes is the largest value that's compressed, and minSize must be
// below it. etcd's default 1.5 MiB request limit leaves 64 KiB for the key, the
// transaction and any encryption envelope (KMS v2 can add 32 KiB of annotations),
// so anything compressed can be rewritten uncompressed. It can drop below the
// read cap in compression's format.go but never rise above it.
const MaxPlaintextBytes = 1536*1024 - 64*1024

// SecretsGroupResource returns core secrets, which no wildcard selects.
// Compressing a Secret tells anyone who can read etcd how compressible its
// plaintext is, so it has to be named.
func SecretsGroupResource() schema.GroupResource {
	return schema.GroupResource{Resource: "secrets"}
}

// CoveringSelectors returns the selectors that could apply to gr, most specific
// first: gr itself, then '*.<group>', then '*.*'. For secrets it returns only
// secrets. Validation rejects masked selectors with it and the loader looks up
// resources with it, so the two agree on which entry decides a resource.
func CoveringSelectors(gr schema.GroupResource) []schema.GroupResource {
	if gr == SecretsGroupResource() {
		return []schema.GroupResource{gr}
	}
	return []schema.GroupResource{gr, {Group: gr.Group, Resource: "*"}, {Group: "*", Resource: "*"}}
}
