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

package etcd3

import (
	"context"
	"testing"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/storage/value/compression"
	"k8s.io/apiserver/pkg/storage/value/encrypt/identity"
)

// TestResourceSizeEstimatorAppliesExpansionFactor is the APF-safety test.
//
// The estimator records len(kv.Value), the stored size. When storage compression
// is in use that is smaller than the plaintext, but the estimate feeds APF's
// bound on the memory a LIST will occupy, and that memory is the decompressed,
// decoded size. Without the correction, a resource compressing 5x would let APF
// admit roughly 5x more concurrent LISTs than the apiserver can hold.
func TestResourceSizeEstimatorAppliesExpansionFactor(t *testing.T) {
	keys := []string{"/prefix/a", "/prefix/b"}
	getKeys := func(ctx context.Context) ([]string, error) { return keys, nil }

	cases := []struct {
		name     string
		factor   func() float64
		wantSize int64
	}{
		{"nil factor is a no-op", nil, 1000},
		{"factor of exactly 1 is a no-op", func() float64 { return 1 }, 1000},
		{"below 1 is ignored, never less conservative", func() float64 { return 0.5 }, 1000},
		{"factor of 5 scales up", func() float64 { return 5 }, 5000},
		{"fractional factor scales up", func() float64 { return 2.5 }, 2500},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			est := newResourceSizeEstimator("/prefix", getKeys, tc.factor)
			defer est.Close()

			for _, k := range keys {
				est.UpdateKey(&mvccpb.KeyValue{
					Key:         []byte(k),
					Value:       make([]byte, 1000),
					ModRevision: 1,
				})
			}

			stats, err := est.Stats(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if stats.EstimatedAverageObjectSizeBytes != tc.wantSize {
				t.Fatalf("EstimatedAverageObjectSizeBytes = %d, want %d",
					stats.EstimatedAverageObjectSizeBytes, tc.wantSize)
			}
			if stats.ObjectCount != int64(len(keys)) {
				t.Errorf("ObjectCount = %d, want %d", stats.ObjectCount, len(keys))
			}
		})
	}
}

// TestCorruptObjTransformerForwardsExpansionFactor guards a silent-failure mode.
// The corrupt-object wrapper embeds value.Transformer, which promotes only that
// interface's methods, so without an explicit forward the type assertion for
// compression.PlaintextSizeExpander would fail whenever AllowUnsafeMalformedObjectDeletion
// is on -- and the APF correction above would quietly disappear on exactly the
// default configuration.
func TestCorruptObjTransformerForwardsExpansionFactor(t *testing.T) {
	t.Run("forwards from a compression transformer", func(t *testing.T) {
		policy, err := compression.NewPolicy(&apiserver.StorageCompressionConfiguration{
			Resources: []apiserver.StorageCompressionResourceConfiguration{{
				Resources: []string{"configmaps"},
				Algorithm: apiserver.CompressionAlgorithmDeflate,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		inner := compression.WithCompression(identity.NewEncryptCheckTransformer(), policy, testGroupResource())
		wrapped := WithCorruptObjErrorHandlingTransformer(inner)

		expander, ok := wrapped.(compression.PlaintextSizeExpander)
		if !ok {
			t.Fatal("the corrupt-object wrapper must expose PlaintextSizeExpander, or the APF size correction is silently lost")
		}
		if got := expander.ExpansionFactor(); got != 1 {
			t.Errorf("ExpansionFactor() = %v before any read, want 1", got)
		}

		// Drive the inner estimate up through the wrapper and confirm it is visible.
		framed, err := inner.TransformToStorage(context.Background(), compressiblePayload(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2000; i++ {
			if _, _, err := wrapped.TransformFromStorage(context.Background(), framed, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got := expander.ExpansionFactor(); got <= 1 {
			t.Fatalf("ExpansionFactor() = %v after compressed reads through the wrapper, want above 1", got)
		}
	})

	t.Run("returns 1 when the delegate does not estimate", func(t *testing.T) {
		wrapped := WithCorruptObjErrorHandlingTransformer(identity.NewEncryptCheckTransformer())
		expander, ok := wrapped.(compression.PlaintextSizeExpander)
		if !ok {
			t.Fatal("expected PlaintextSizeExpander")
		}
		if got := expander.ExpansionFactor(); got != 1 {
			t.Errorf("ExpansionFactor() = %v, want 1", got)
		}
	})
}

func testGroupResource() schema.GroupResource {
	return schema.GroupResource{Group: "", Resource: "configmaps"}
}

func compressiblePayload() []byte {
	out := make([]byte, 0, 8192)
	for len(out) < 8192 {
		out = append(out, []byte("kubernetes-api-object-payload-")...)
	}
	return out
}
