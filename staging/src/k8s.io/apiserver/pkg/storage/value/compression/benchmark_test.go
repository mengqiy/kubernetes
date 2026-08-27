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
	"fmt"
	"testing"
)

// Payload shapes chosen to bracket what actually lives in etcd.
//
// The ratios these produce are indicative, not evidence. Do NOT quote them as
// the motivation for the feature: they are synthetic, and a payload built by
// repeating one identical document compresses absurdly well (well over 100x) in
// a way no real object does. The generators below therefore vary the content of
// every repetition, which lands them in the same neighbourhood as real API
// objects, but the only number worth putting in a KEP comes from compressing an
// actual etcd dump. What these benchmarks are genuinely good for is the CPU cost
// per byte and as a regression guard on it.
func benchPayloads() map[string][]byte {
	// jsonObjects emits distinct ConfigMap-shaped JSON documents, so the
	// repetition is structural (field names, boilerplate) rather than literal.
	jsonObjects := func(size int) []byte {
		var buf bytes.Buffer
		for i := 0; buf.Len() < size; i++ {
			fmt.Fprintf(&buf,
				`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-%d-%x","namespace":"ns-%d","uid":"%x-%x-%x","resourceVersion":"%d","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"k%d\":\"%x\"}}"},"managedFields":[{"manager":"kubectl-client-side-apply","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1","time":"2026-08-%02dT%02d:%02d:00Z"}]},"data":{"key-%d":"%x"}}`,
				i, i*2654435761, i%64, i*7, i*11, i*13, 1000000+i*7, i, i*99991, i%28+1, i%24, i%60, i, i*104729)
		}
		return buf.Bytes()[:size]
	}
	// protoLike emits length-prefixed-ish binary with realistic string content and
	// a fraction of high-entropy bytes, standing in for protobuf storage.
	protoLike := func(size int) []byte {
		var buf bytes.Buffer
		buf.Write([]byte{0x6b, 0x38, 0x73, 0x00})
		for i := 0; buf.Len() < size; i++ {
			fmt.Fprintf(&buf, "\n\x1fdefault\x12\x0fconfigmap-%d\x1a\x24", i)
			buf.Write(randomBytes(16))
		}
		return buf.Bytes()[:size]
	}

	return map[string][]byte{
		"512B/protolike":   protoLike(512),
		"1KiB/protolike":   protoLike(1024),
		"4KiB/protolike":   protoLike(4096),
		"4KiB/json":        jsonObjects(4096),
		"64KiB/json":       jsonObjects(65536),
		"64KiB/random":     randomBytes(65536),
		"1MiB/json":        jsonObjects(1 << 20),
		"1.5MiB/protolike": protoLike(MaxPlaintextBytes),
	}
}

func BenchmarkDeflateFrame(b *testing.B) {
	for name, payload := range benchPayloads() {
		b.Run(name, func(b *testing.B) {
			framed, err := deflateFrame(payload)
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := deflateFrame(payload); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()

			// Reported after the loop: ResetTimer discards custom metrics that
			// were reported before it. Stored bytes divided by plaintext bytes,
			// so lower is better. This is the number the motivation rests on, and
			// having it in the benchmark output is how it stays honest.
			b.ReportMetric(float64(len(framed))/float64(len(payload)), "stored_over_plaintext")
		})
	}
}

func BenchmarkUnframeDeflate(b *testing.B) {
	ctx := context.Background()
	for name, payload := range benchPayloads() {
		b.Run(name, func(b *testing.B) {
			framed, err := deflateFrame(payload)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := unframe(ctx, framed); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkUnframeLegacy measures the cost this feature imposes on a cluster that
// never enables it. Because the decorator is installed unconditionally, this runs
// on every read in every cluster, so it needs to be indistinguishable from noise.
func BenchmarkUnframeLegacy(b *testing.B) {
	ctx := context.Background()
	payload := benchPayloads()["64KiB/json"]
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := unframe(ctx, payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTransformer measures the whole decorator, which is what a write or read
// actually pays, with the policy on and off.
func BenchmarkTransformer(b *testing.B) {
	ctx := context.Background()
	payloads := benchPayloads()

	for _, enabled := range []bool{false, true} {
		var policy *Policy
		if enabled {
			p, err := NewPolicy(config(entry(deflate, "configmaps")))
			if err != nil {
				b.Fatal(err)
			}
			policy = p
		}
		tr := WithCompression(newRecording(), policy, testGR)

		for name, payload := range payloads {
			b.Run(fmt.Sprintf("enabled=%v/%s/write", enabled, name), func(b *testing.B) {
				b.SetBytes(int64(len(payload)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := tr.TransformToStorage(ctx, payload, testDataCtx); err != nil {
						b.Fatal(err)
					}
				}
			})

			stored, err := tr.TransformToStorage(ctx, payload, testDataCtx)
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("enabled=%v/%s/read", enabled, name), func(b *testing.B) {
				b.SetBytes(int64(len(payload)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := tr.TransformFromStorage(ctx, stored, testDataCtx); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
