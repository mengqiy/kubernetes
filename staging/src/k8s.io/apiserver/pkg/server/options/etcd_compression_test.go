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

package options

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	storagefactory "k8s.io/apiserver/pkg/storage/storagebackend/factory"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage-compression.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["events", "events.events.k8s.io"]
    algorithm: None
  - resources: ["*.*"]
    algorithm: Deflate
`

// TestValidateStorageCompression covers every --storage-compression-config
// validation rule. These error strings are the primary operator-facing
// documentation of the contract, so they are asserted rather than just the fact of
// failure.
func TestValidateStorageCompression(t *testing.T) {
	testCases := []struct {
		name        string
		gateEnabled bool
		contents    string
		noFile      bool
		expectErr   string
	}{
		{
			name:        "no file is always valid, gate off",
			noFile:      true,
			gateEnabled: false,
		},
		{
			name:        "no file is always valid, gate on",
			noFile:      true,
			gateEnabled: true,
		},
		{
			name:        "valid config with the gate on",
			gateEnabled: true,
			contents:    validConfig,
		},
		{
			name:        "config requires the gate",
			gateEnabled: false,
			contents:    validConfig,
			expectErr:   "requires --feature-gates=StorageObjectCompression=true",
		},
		{
			name:        "empty file is rejected",
			gateEnabled: true,
			contents:    "",
			expectErr:   "empty",
		},
		{
			name:        "unparseable file is rejected",
			gateEnabled: true,
			contents:    "this is not yaml: [",
			expectErr:   "--storage-compression-config",
		},
		{
			name:        "wrong kind is rejected",
			gateEnabled: true,
			contents:    "apiVersion: apiserver.config.k8s.io/v1alpha1\nkind: EncryptionConfiguration\nresources: []\n",
			expectErr:   "--storage-compression-config",
		},
		{
			name:        "unknown field is rejected by strict decoding",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["configmaps"]
    algorithm: Deflate
    minSizeBytes: 4096
`,
			expectErr: "--storage-compression-config",
		},
		{
			name:        "no resources is rejected",
			gateEnabled: true,
			contents:    "apiVersion: apiserver.config.k8s.io/v1alpha1\nkind: StorageCompressionConfiguration\nresources: []\n",
			expectErr:   "at least one",
		},
		{
			name:        "unsupported algorithm is rejected",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["configmaps"]
    algorithm: Zstd
`,
			expectErr: "unsupported algorithm",
		},
		{
			name:        "capital letters in a resource are rejected",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["ConfigMaps"]
`,
			expectErr: "capital letters",
		},
		{
			name:        "a bare star is rejected",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["*"]
`,
			expectErr: "use '*.'",
		},
		{
			name:        "overlap within one entry is rejected",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["secrets", "*."]
`,
			expectErr: "overlapping",
		},
		{
			// Ordering is the exclusion mechanism, so this must be allowed.
			name:        "overlap ACROSS entries is allowed when the earlier one is more specific",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["configmaps"]
    algorithm: None
  - resources: ["*.*"]
    algorithm: Deflate
`,
		},
		{
			name:        "a fully shadowed entry is rejected as dead configuration",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["*.*"]
    algorithm: Deflate
  - resources: ["configmaps"]
    algorithm: None
`,
			expectErr: "has no effect",
		},
		{
			// No wildcard reaches secrets, so this is meaningful, not dead.
			name:        "naming secrets after a wildcard is not shadowed",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["*.*"]
    algorithm: Deflate
  - resources: ["secrets"]
    algorithm: Deflate
`,
		},
		{
			// events.events.k8s.io must be usable, unlike in EncryptionConfiguration,
			// because excluding events is the main reason to write an exclusion entry.
			name:        "the events.k8s.io group is allowed",
			gateEnabled: true,
			contents: `
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["events.events.k8s.io"]
    algorithm: None
  - resources: ["*.*"]
    algorithm: Deflate
`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StorageObjectCompression, tc.gateEnabled)

			opts := &EtcdOptions{}
			if !tc.noFile {
				opts.StorageConfig.StorageCompressionConfigFile = writeConfig(t, tc.contents)
			}

			errs := opts.validateStorageCompression()

			if tc.expectErr == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no error, got %v", errs)
				}
				return
			}

			var found bool
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.expectErr) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected an error containing %q, got %v", tc.expectErr, errs)
			}
		})
	}
}

func TestStorageCompressionFlagIsRegistered(t *testing.T) {
	opts := NewEtcdOptions(&storagebackend.Config{})
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.AddFlags(fs)

	if fs.Lookup("storage-compression-config") == nil {
		t.Fatal("--storage-compression-config is not registered")
	}
	// The superseded flags must be gone: configuration is a file now.
	for _, gone := range []string{"storage-compression", "storage-compression-include-secrets"} {
		if fs.Lookup(gone) != nil {
			t.Errorf("--%s should no longer exist", gone)
		}
	}

	if err := fs.Parse([]string{"--storage-compression-config=/etc/kubernetes/sc.yaml"}); err != nil {
		t.Fatal(err)
	}
	if got, want := opts.StorageConfig.StorageCompressionConfigFile, "/etc/kubernetes/sc.yaml"; got != want {
		t.Errorf("StorageCompressionConfigFile = %q, want %q", got, want)
	}
}

// TestCompressionConfigPathPropagatesToEveryConsumer is the plumbing test.
//
// The path lives on storagebackend.Config rather than being resolved during
// ApplyTo, because several consumers copy StorageConfig without ever calling
// ApplyWithStorageFactoryTo. NewCRDRESTOptionsGetter is the important one: it
// copies EtcdOptions and calls CreateRESTOptionsGetter directly, so a policy
// injected by decorating the factory in ApplyWithStorageFactoryTo would never
// reach custom resources -- which are the largest beneficiary of compression.
func TestCompressionConfigPathPropagatesToEveryConsumer(t *testing.T) {
	path := writeConfig(t, validConfig)
	opts := NewEtcdOptions(&storagebackend.Config{})
	opts.StorageConfig.StorageCompressionConfigFile = path

	// A per-resource config obtained the way the generic path does.
	factory := &SimpleStorageFactory{StorageConfig: opts.StorageConfig}
	cfg, err := factory.NewConfig(schema.GroupResource{Resource: "configmaps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageCompressionConfigFile != path {
		t.Errorf("SimpleStorageFactory dropped the path: got %q, want %q", cfg.StorageCompressionConfigFile, path)
	}

	// And the way custom resources do: a plain value copy of StorageConfig.
	crCopy := opts.StorageConfig
	if crCopy.StorageCompressionConfigFile != path {
		t.Errorf("a value copy of StorageConfig dropped the path: got %q", crCopy.StorageCompressionConfigFile)
	}
}

// TestLoadStorageCompressionPolicyIsCached checks that the file is read once per
// path. The path reaches hundreds of per-resource configs, so resolving it per
// store would be hundreds of redundant reads at startup.
func TestLoadStorageCompressionPolicyIsCached(t *testing.T) {
	path := writeConfig(t, validConfig)

	first, err := storagefactory.LoadStorageCompressionPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("expected a policy")
	}

	// Remove the file: a cached result must still be returned.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second, err := storagefactory.LoadStorageCompressionPolicy(path)
	if err != nil {
		t.Fatalf("a cached policy must not be re-read from disk: %v", err)
	}
	if second != first {
		t.Error("expected the identical cached policy pointer")
	}
}

func TestLoadStorageCompressionPolicyEmptyPath(t *testing.T) {
	policy, err := storagefactory.LoadStorageCompressionPolicy("")
	if err != nil {
		t.Fatal(err)
	}
	if policy != nil {
		t.Fatal("an empty path must yield a nil policy")
	}
	if !policy.IsEmpty() {
		t.Fatal("a nil policy must be inert")
	}
}
