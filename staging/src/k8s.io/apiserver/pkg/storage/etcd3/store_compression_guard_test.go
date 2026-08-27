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
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/apis/example"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/apiserver/pkg/storage/value/compression"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
)

// sentinelTransformer returns a configurable error from TransformFromStorage so a
// test can simulate a specific storage-layer read failure.
type sentinelTransformer struct {
	value.Transformer
	err atomic.Pointer[error]
}

func (t *sentinelTransformer) setErr(err error) {
	if err == nil {
		t.err.Store(nil)
		return
	}
	t.err.Store(&err)
}

func (t *sentinelTransformer) TransformFromStorage(ctx context.Context, data []byte, dataCtx value.Context) ([]byte, bool, error) {
	if e := t.err.Load(); e != nil {
		return nil, false, *e
	}
	return t.Transformer.TransformFromStorage(ctx, data, dataCtx)
}

// TestUnsafeDeleteRefusedForStorageFormatErrors is the data-loss guard for storage
// compression, exercised end to end against a real etcd.
//
// `kubectl delete --ignore-store-read-errors-with-cluster-breaking-potential`
// reaches store.Delete with ExpectTransformOrDecodeError set, and getState would
// otherwise treat ANY transform error as license to delete. A compression frame
// this apiserver cannot interpret is version skew, not corruption: the object is
// readable by a newer apiserver, so deleting it destroys good data. The guard must
// refuse the delete regardless of the option, and it must do so without breaking
// the genuine corrupt-object case that the unsafe deleter exists to serve.
func TestUnsafeDeleteRefusedForStorageFormatErrors(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AllowUnsafeMalformedObjectDeletion, true)

	cases := []struct {
		name         string
		readErr      error
		wantRefused  bool
		wantSentinel error
	}{
		{
			name:         "unsupported compression algorithm is refused",
			readErr:      fmt.Errorf("%w: algorithm 0xfe", compression.ErrUnsupportedCompressionAlgorithm),
			wantRefused:  true,
			wantSentinel: compression.ErrUnsupportedCompressionAlgorithm,
		},
		{
			name:         "malformed compression frame is refused",
			readErr:      fmt.Errorf("%w: bad magic", compression.ErrCompressionFormat),
			wantRefused:  true,
			wantSentinel: compression.ErrCompressionFormat,
		},
		{
			// The pre-existing behaviour the unsafe deleter exists for must be
			// preserved, otherwise this guard would have broken the feature.
			name:        "a genuine transform failure is still deletable",
			readErr:     errors.New("cipher: message authentication failed"),
			wantRefused: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transformer := &sentinelTransformer{Transformer: newTestTransformer()}
			ctx, s, _ := testSetup(t, withTransformer(transformer))

			key := "/pods/ns/foo"
			pod := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo"}}
			if err := s.Create(ctx, key, pod, &example.Pod{}, 0); err != nil {
				t.Fatalf("create: %v", err)
			}

			// Make reads fail the way the scenario under test would.
			transformer.setErr(tc.readErr)

			err := s.Delete(ctx, key, &example.Pod{}, nil, storage.ValidateAllObjectFunc, nil,
				storage.DeleteOptions{ExpectTransformOrDecodeError: true})

			if tc.wantRefused {
				if err == nil {
					t.Fatal("expected the unsafe delete to be refused, but it succeeded")
				}
				if !errors.Is(err, tc.wantSentinel) {
					t.Fatalf("error must preserve the sentinel so an operator can tell skew from corruption, got %v", err)
				}

				// The object must still be there. Restore healthy reads and look.
				transformer.setErr(nil)
				if err := s.Get(ctx, key, storage.GetOptions{}, &example.Pod{}); err != nil {
					t.Fatalf("the object must survive a refused unsafe delete, but Get failed: %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("a genuine corrupt object must remain unsafe-deletable, got %v", err)
			}
			transformer.setErr(nil)
			if err := s.Get(ctx, key, storage.GetOptions{}, &example.Pod{}); err == nil {
				t.Fatal("expected the object to have been deleted")
			}
		})
	}
}
