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

package factory

import (
	"fmt"
	"sync"

	"k8s.io/apiserver/pkg/apis/apiserver/load"
	apiservervalidation "k8s.io/apiserver/pkg/apis/apiserver/validation"
	"k8s.io/apiserver/pkg/storage/value/compression"
	"k8s.io/klog/v2"
)

// compressionPolicyCache memoizes the resolved policy per configuration path.
//
// The path travels on storagebackend.Config, which is copied into every
// StorageFactory and every per-resource config, so a store is constructed for
// each of hundreds of resources. Reading and validating the file once per store
// would be hundreds of redundant file reads at startup, so the result -- success
// or failure -- is cached. The configuration is read once and never reloaded, so
// caching it is not merely an optimisation but a statement of the semantics.
var compressionPolicyCache sync.Map // path string -> *cachedCompressionPolicy

type cachedCompressionPolicy struct {
	policy *compression.Policy
	err    error
}

// LoadStorageCompressionPolicy loads, validates and resolves a storage
// compression configuration file. An empty path yields a nil policy, which
// selects nothing.
//
// The result is cached per path, so calling this during option validation for
// fail-fast behaviour costs nothing when the storage layer later asks for the
// same file.
func LoadStorageCompressionPolicy(path string) (*compression.Policy, error) {
	if len(path) == 0 {
		return nil, nil
	}
	if v, ok := compressionPolicyCache.Load(path); ok {
		cached := v.(*cachedCompressionPolicy)
		return cached.policy, cached.err
	}

	policy, err := loadStorageCompressionPolicy(path)
	// LoadOrStore rather than Store so that concurrent callers agree on one
	// result, including one warning rather than several.
	actual, loaded := compressionPolicyCache.LoadOrStore(path, &cachedCompressionPolicy{policy: policy, err: err})
	if loaded {
		cached := actual.(*cachedCompressionPolicy)
		return cached.policy, cached.err
	}
	return policy, err
}

func loadStorageCompressionPolicy(path string) (*compression.Policy, error) {
	config, err := load.LoadStorageCompressionConfigurationFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading storage compression configuration: %w", err)
	}

	if errs := apiservervalidation.ValidateStorageCompressionConfiguration(config); len(errs) > 0 {
		return nil, fmt.Errorf("invalid storage compression configuration: %w", errs.ToAggregate())
	}

	policy, err := compression.NewPolicy(config)
	if err != nil {
		return nil, fmt.Errorf("invalid storage compression configuration: %w", err)
	}

	// Ordering is the exclusion mechanism, so overlap between entries is expected
	// and legal. An entry entirely shadowed by an earlier one, however, is dead
	// configuration and almost always a misordered file, which would otherwise
	// silently do nothing.
	for _, shadowed := range policy.ShadowedSelectors(config) {
		return nil, fmt.Errorf("invalid storage compression configuration: %s", shadowed)
	}

	// Naming secrets explicitly is the acknowledgement; no wildcard selects it. Say
	// plainly what that costs rather than refusing, since the operator has already
	// been deliberate.
	if policy.EnabledFor(compression.SecretsGroupResource) {
		klog.Warningf("storage compression is enabled for %q: compressing a Secret before encrypting it "+
			"reveals the compressibility of its plaintext to anyone able to read etcd or an etcd backup, "+
			"which is the adversary that encryption at rest defends against",
			compression.SecretsGroupResource.String())
	}

	return policy, nil
}
