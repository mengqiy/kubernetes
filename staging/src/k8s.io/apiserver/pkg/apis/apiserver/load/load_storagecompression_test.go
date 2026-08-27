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

package load

import (
	"strings"
	"testing"

	api "k8s.io/apiserver/pkg/apis/apiserver"
)

// TestLoadStorageCompressionConfiguration exercises registration, decoding,
// defaulting and conversion together: if the kind were not registered in both the
// internal and v1alpha1 schemes, or the generated conversions were stale, this
// fails.
func TestLoadStorageCompressionConfiguration(t *testing.T) {
	t.Run("decodes and preserves order", func(t *testing.T) {
		cfg, err := LoadStorageCompressionConfigurationFromData([]byte(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["events", "events.events.k8s.io"]
    algorithm: None
  - resources: ["*.*"]
    algorithm: Deflate
`))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Resources) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(cfg.Resources))
		}
		// Order carries the semantics, so it must survive conversion.
		if cfg.Resources[0].Algorithm != api.CompressionAlgorithmNone {
			t.Errorf("entry 0 algorithm = %q, want None", cfg.Resources[0].Algorithm)
		}
		if got, want := cfg.Resources[0].Resources, []string{"events", "events.events.k8s.io"}; !equal(got, want) {
			t.Errorf("entry 0 resources = %v, want %v", got, want)
		}
		if cfg.Resources[1].Algorithm != api.CompressionAlgorithmDeflate {
			t.Errorf("entry 1 algorithm = %q, want Deflate", cfg.Resources[1].Algorithm)
		}
	})

	t.Run("omitted algorithm defaults to Deflate", func(t *testing.T) {
		cfg, err := LoadStorageCompressionConfigurationFromData([]byte(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["configmaps"]
`))
		if err != nil {
			t.Fatal(err)
		}
		// An entry names resources in order to compress them, so Deflate is the
		// intent-preserving default. If defaulting were not wired up this would be
		// the empty string and the entry would silently do nothing.
		if got := cfg.Resources[0].Algorithm; got != api.CompressionAlgorithmDeflate {
			t.Fatalf("algorithm = %q, want %q", got, api.CompressionAlgorithmDeflate)
		}
	})

	t.Run("empty data is an error rather than a silent default", func(t *testing.T) {
		if _, err := LoadStorageCompressionConfigurationFromData(nil); err == nil {
			t.Fatal("expected an error for empty data")
		}
	})

	t.Run("unknown field is rejected by strict decoding", func(t *testing.T) {
		_, err := LoadStorageCompressionConfigurationFromData([]byte(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: StorageCompressionConfiguration
resources:
  - resources: ["configmaps"]
    algorithmm: Deflate
`))
		if err == nil {
			t.Fatal("expected strict decoding to reject an unknown field")
		}
		if !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("expected an unknown-field error, got %v", err)
		}
	})

	t.Run("wrong kind is rejected", func(t *testing.T) {
		_, err := LoadStorageCompressionConfigurationFromData([]byte(`
apiVersion: apiserver.config.k8s.io/v1alpha1
kind: TracingConfiguration
`))
		if err == nil {
			t.Fatal("expected an error for the wrong kind")
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := LoadStorageCompressionConfigurationFromFile("/nonexistent/storage-compression.yaml"); err == nil {
			t.Fatal("expected an error for a missing file")
		}
	})
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
