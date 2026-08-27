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

package etcd3

import (
	"context"
	"errors"
	"fmt"
	"testing"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/apiserver/pkg/storage/value/compression"
)

func TestAggregatedStorageErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		abortErr error
		want     string
	}{
		{
			name: "aggregation completed without an abort",
			want: "unable to transform or decode 2 objects: {\n\tfoo is corrupt\n\tbar is corrupt\n}",
		},
		{
			name:     "aggregation aborted after reaching the limit",
			abortErr: errTooMany,
			want:     "unable to transform or decode 2 objects: {\n\tfoo is corrupt\n\tbar is corrupt\n}, aborted: too many errors, the list is truncated",
		},
		{
			name:     "aggregation aborted due to an unexpected error",
			abortErr: errors.New("etcd is down"),
			want:     "unable to transform or decode 2 objects: {\n\tfoo is corrupt\n\tbar is corrupt\n}, aborted: etcd is down",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := &aggregatedStorageError{
				resourcePrefix: "list",
				errs:           utilerrors.NewAggregate([]error{errors.New("foo is corrupt"), errors.New("bar is corrupt")}),
				abortErr:       test.abortErr,
			}
			if want, got := test.want, err.Error(); want != got {
				t.Errorf("unexpected error message, want:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}

// erroringTransformer returns a fixed error from TransformFromStorage.
type erroringTransformer struct {
	err error
}

func (e erroringTransformer) TransformFromStorage(context.Context, []byte, value.Context) ([]byte, bool, error) {
	return nil, false, e.err
}

func (e erroringTransformer) TransformToStorage(_ context.Context, data []byte, _ value.Context) ([]byte, error) {
	return data, nil
}

// TestStorageFormatErrorsAreNotClassifiedAsCorruption is the data-loss guard.
//
// A compression frame this apiserver cannot interpret means a newer apiserver
// wrote it, so the object is readable -- just not here. Classifying that as a
// corrupt object surfaces it to the client as a CorruptObjError, which is exactly
// the signal that invites an operator to run
// `kubectl delete --ignore-store-read-errors-with-cluster-breaking-potential` and
// destroy an object that was never damaged.
func TestStorageFormatErrorsAreNotClassifiedAsCorruption(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantCorrupt bool
	}{
		{
			name:        "unsupported compression algorithm",
			err:         fmt.Errorf("%w: algorithm 0xfe", compression.ErrUnsupportedCompressionAlgorithm),
			wantCorrupt: false,
		},
		{
			name:        "malformed compression frame",
			err:         fmt.Errorf("%w: bad magic", compression.ErrCompressionFormat),
			wantCorrupt: false,
		},
		{
			name:        "genuine decryption failure is still corruption",
			err:         errors.New("cipher: message authentication failed"),
			wantCorrupt: true,
		},
		{
			name:        "unknown error is still corruption",
			err:         errors.New("something else went wrong"),
			wantCorrupt: true,
		},

		// The reason classification uses a sentinel with errors.Is rather than a
		// typed error with errors.As. prefixTransformers returns
		// utilerrors.NewAggregate when more than one provider matches the same
		// prefix -- which is the ordinary state of a cluster mid key-rotation,
		// where two aescbc blocks share "k8s:enc:aescbc:v1:". That aggregate
		// implements Is but has neither As nor Unwrap, so an errors.As-based
		// check silently fails in exactly the configuration most likely to be
		// skewed.
		{
			name: "sentinel survives an aggregate of one",
			err: utilerrors.NewAggregate([]error{
				fmt.Errorf("%w: algorithm 0xfe", compression.ErrUnsupportedCompressionAlgorithm),
			}),
			wantCorrupt: false,
		},
		{
			name: "sentinel survives an aggregate of several (key rotation)",
			err: utilerrors.NewAggregate([]error{
				errors.New("key1: decryption failed"),
				fmt.Errorf("%w: algorithm 0xfe", compression.ErrUnsupportedCompressionAlgorithm),
			}),
			wantCorrupt: false,
		},
		{
			name: "an aggregate with no sentinel is still corruption",
			err: utilerrors.NewAggregate([]error{
				errors.New("key1: decryption failed"),
				errors.New("key2: decryption failed"),
			}),
			wantCorrupt: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transformer := WithCorruptObjErrorHandlingTransformer(erroringTransformer{err: tc.err})
			_, _, err := transformer.TransformFromStorage(context.Background(), []byte("data"), nil)
			if err == nil {
				t.Fatal("expected an error")
			}

			var corruptErr *corruptObjectError
			gotCorrupt := errors.As(err, &corruptErr)
			if gotCorrupt != tc.wantCorrupt {
				t.Fatalf("classified as corrupt object = %v, want %v (err: %v)", gotCorrupt, tc.wantCorrupt, err)
			}

			// A non-corrupt classification must still preserve the original
			// error, so the storage layer and the operator both see the reason.
			if !tc.wantCorrupt && !errors.Is(err, tc.err) && !isRecoverableStorageFormatError(err) {
				t.Fatalf("the original error must be preserved, got %v", err)
			}
		})
	}
}

// TestAggregateDoesNotSupportErrorsAs pins the apimachinery behaviour the guard
// above depends on. If utilerrors.aggregate ever gains an As or Unwrap method
// this test will fail, at which point the errors.Is choice can be revisited --
// but until then, switching to errors.As would be a silent regression.
func TestAggregateDoesNotSupportErrorsAs(t *testing.T) {
	sentinel := fmt.Errorf("%w: algorithm 0xfe", compression.ErrUnsupportedCompressionAlgorithm)
	agg := utilerrors.NewAggregate([]error{errors.New("other"), sentinel})

	if !errors.Is(agg, compression.ErrUnsupportedCompressionAlgorithm) {
		t.Error("errors.Is must traverse a utilerrors aggregate; the classification guard depends on it")
	}

	// Demonstrate why errors.As would be the wrong tool here.
	type probe struct{ error }
	var target *probe
	if errors.As(agg, &target) {
		t.Error("unexpected: aggregate now supports errors.As")
	}
}

// TestRecoverableStorageFormatErrorAbortsList documents the deliberate LIST
// consequence of not classifying these as corrupt objects: the aggregator treats
// a non-corruptObjectError as a reason to abort the whole list rather than to
// aggregate per item. One loud failure meaning "upgrade this apiserver" is the
// intended trade, but it must be a choice rather than an accident.
func TestRecoverableStorageFormatErrorAbortsList(t *testing.T) {
	agg := corruptObjErrAggregatorFactory(10)()

	err := fmt.Errorf("%w: algorithm 0xfe", compression.ErrUnsupportedCompressionAlgorithm)
	if abort := agg.Append("/registry/configmaps/default/a", err); !abort {
		t.Fatal("a recoverable storage-format error should abort the list rather than be aggregated per item")
	}
	if got := agg.Aggregate(); !errors.Is(got, compression.ErrUnsupportedCompressionAlgorithm) {
		t.Fatalf("the abort error must preserve the sentinel, got %v", got)
	}
}
