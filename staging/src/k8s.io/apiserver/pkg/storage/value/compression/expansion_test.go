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

package compression

import (
	"bytes"
	"context"
	"testing"
)

// TestExpansionFactorStartsAtOne is the property that makes this safe to ship:
// before anything has been observed, and forever on a cluster that never enables
// compression, the correction must be exactly 1.0 so APF accounting is unchanged.
func TestExpansionFactorStartsAtOne(t *testing.T) {
	for name, policy := range map[string]*Policy{
		"nil policy":     nil,
		"policy on":      policyFor(t, "configmaps"),
		"other resource": policyFor(t, "pods"),
	} {
		t.Run(name, func(t *testing.T) {
			tr := WithCompression(newRecording(), policy, testGR).(PlaintextSizeExpander)
			if got := tr.ExpansionFactor(); got != 1 {
				t.Fatalf("ExpansionFactor() = %v before any read, want exactly 1", got)
			}
		})
	}
}

// TestExpansionFactorStaysAtOneForLegacyReads covers the case that matters most in
// practice: a resource whose stored values are all uncompressed must not have its
// APF accounting perturbed at all, even with the policy switched on.
func TestExpansionFactorStaysAtOneForLegacyReads(t *testing.T) {
	tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
	expander := tr.(PlaintextSizeExpander)

	for i := 0; i < 500; i++ {
		if _, _, err := tr.TransformFromStorage(context.Background(), compressible, testDataCtx); err != nil {
			t.Fatal(err)
		}
	}
	if got := expander.ExpansionFactor(); got != 1 {
		t.Fatalf("ExpansionFactor() = %v after 500 uncompressed reads, want exactly 1", got)
	}
}

// TestExpansionFactorConvergesOnCompressedReads checks that the estimate does rise
// once compressed values are actually being read, which is what protects APF from
// under-charging LIST memory by the compression ratio.
func TestExpansionFactorConvergesOnCompressedReads(t *testing.T) {
	tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
	expander := tr.(PlaintextSizeExpander)

	stored, err := tr.TransformToStorage(context.Background(), compressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}
	framed := mustDeflate(t, compressible)
	trueRatio := float64(len(compressible)) / float64(len(framed))
	if trueRatio < 2 {
		t.Fatalf("test precondition: expected the payload to compress at least 2x, got %.2f", trueRatio)
	}

	for i := 0; i < 2000; i++ {
		if _, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx); err != nil {
			t.Fatal(err)
		}
	}

	got := expander.ExpansionFactor()
	if got < trueRatio*0.9 || got > trueRatio*1.1 {
		t.Fatalf("ExpansionFactor() = %.2f after 2000 compressed reads, want ~%.2f", got, trueRatio)
	}
}

// TestExpansionFactorTracksTheStoredMix is the reason the EWMA samples reads
// rather than writes. On a half-migrated resource the factor must land between
// 1.0 and the full ratio, because that is what the population actually costs.
func TestExpansionFactorTracksTheStoredMix(t *testing.T) {
	tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
	expander := tr.(PlaintextSizeExpander)

	compressedValue, err := tr.TransformToStorage(context.Background(), compressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}
	legacyValue := compressible

	for i := 0; i < 4000; i++ {
		v := legacyValue
		if i%2 == 0 {
			v = compressedValue
		}
		if _, _, err := tr.TransformFromStorage(context.Background(), v, testDataCtx); err != nil {
			t.Fatal(err)
		}
	}

	full := float64(len(compressible)) / float64(len(mustDeflate(t, compressible)))
	got := expander.ExpansionFactor()
	if got <= 1.05 {
		t.Fatalf("ExpansionFactor() = %.2f on a half-compressed population, want above 1", got)
	}
	if got >= full {
		t.Fatalf("ExpansionFactor() = %.2f on a half-compressed population, want below the full ratio %.2f", got, full)
	}
}

// TestExpansionFactorNeverBelowOne guards the floor. Framing overhead on an
// incompressible value makes stored slightly larger than plaintext, which must not
// be allowed to make APF less conservative than measuring stored bytes alone.
func TestExpansionFactorNeverBelowOne(t *testing.T) {
	tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
	expander := tr.(PlaintextSizeExpander)

	incompressible := randomBytes(4096)
	stored, err := tr.TransformToStorage(context.Background(), incompressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 500; i++ {
		if _, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx); err != nil {
			t.Fatal(err)
		}
	}
	if got := expander.ExpansionFactor(); got < 1 {
		t.Fatalf("ExpansionFactor() = %v, must never drop below 1", got)
	}
}

func TestObserveExpansionEdgeCases(t *testing.T) {
	tr := WithCompression(newRecording(), nil, testGR).(*transformer)

	// Must not panic or produce NaN/Inf on degenerate inputs.
	tr.observeExpansion(0, 0)
	tr.observeExpansion(100, 0)
	tr.observeExpansion(-1, 10)
	tr.observeExpansion(0, 10)
	if got := tr.ExpansionFactor(); got != 1 {
		t.Fatalf("ExpansionFactor() = %v after degenerate samples, want 1", got)
	}

	// A zero-length read of a zero-length stored value must be ignored, not
	// treated as an infinite ratio.
	if _, _, err := tr.TransformFromStorage(context.Background(), []byte{}, testDataCtx); err != nil {
		t.Fatal(err)
	}
	if got := tr.ExpansionFactor(); got != 1 {
		t.Fatalf("ExpansionFactor() = %v after an empty read, want 1", got)
	}
}

func TestConcurrentExpansionObservation(t *testing.T) {
	tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
	expander := tr.(PlaintextSizeExpander)

	stored, err := tr.TransformToStorage(context.Background(), bytes.Repeat([]byte("abcd"), 512), testDataCtx)
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 16
	done := make(chan struct{}, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < 200; i++ {
				if _, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx); err != nil {
					panic(err)
				}
				_ = expander.ExpansionFactor()
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < goroutines; g++ {
		<-done
	}
	if got := expander.ExpansionFactor(); got < 1 {
		t.Fatalf("ExpansionFactor() = %v after concurrent observation, want >= 1", got)
	}
}
