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
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

// FuzzUnframe treats the frame parser as what it is: the first apiserver code
// path that allocates based on length metadata read from storage.
//
// AES-CBC and the KMS v1 CBC read fallback are unauthenticated and malleable, and
// the identity provider does not authenticate at all, so an actor able to tamper
// with etcd can steer these bytes without holding any key. The parser must
// therefore never panic, never allocate beyond MaxDecompressedBytes, and always
// either return a plaintext of exactly the declared length or one of the two
// sentinels.
func FuzzUnframe(f *testing.F) {
	// Well-formed frames.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x00, 0x6b, 0x38, 0x73, algStored})
	f.Add(append([]byte{0x00, 0x6b, 0x38, 0x73, algStored}, []byte("plaintext")...))
	if framed, err := deflateFrame(bytes.Repeat([]byte("kubernetes"), 200)); err == nil {
		f.Add(framed)
	}
	if framed, err := deflateFrame([]byte("small")); err == nil {
		f.Add(framed)
	}

	// Legacy shapes that must pass through.
	f.Add([]byte(`{"apiVersion":"v1"}`))
	f.Add([]byte{0x6b, 0x38, 0x73, 0x00, 0x0a, 0x01, 0x41})
	f.Add([]byte{0xd9, 0xd9, 0xf7, 0xa0})

	// Hostile shapes.
	f.Add([]byte{0x00, 0x6b, 0x38, 0x73, 0xff})
	f.Add([]byte{0x00, 0x6b, 0x38, 0x73, algDeflate})
	f.Add([]byte{0x00, 0x6b, 0x38, 0x73, algDeflate, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
	f.Add([]byte{0x00, 0x6b, 0x38, 0x73, algDeflate, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})
	f.Add([]byte{0x00, 'k', '8', 's', algDeflate, 0x01, 0xff, 0xff, 0xff})

	// A genuine bomb: a small stream declaring a small length but inflating large.
	{
		var buf bytes.Buffer
		if zw, err := flate.NewWriter(&buf, deflateLevel); err == nil {
			_, _ = zw.Write(make([]byte, 4<<20))
			_ = zw.Close()
			var hdr [headerSize + binary.MaxVarintLen64]byte
			copy(hdr[:], frameMagic[:])
			hdr[4] = algDeflate
			w := binary.PutUvarint(hdr[headerSize:], 32)
			f.Add(append(append([]byte(nil), hdr[:headerSize+w]...), buf.Bytes()...))
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// A copy, so we can detect mutation of the caller's buffer.
		original := append([]byte(nil), data...)

		out, kind, err := unframe(context.Background(), data)

		if !bytes.Equal(data, original) {
			t.Fatalf("unframe mutated its input:\n got % x\nwant % x", data, original)
		}

		if err != nil {
			if !errors.Is(err, ErrCompressionFormat) && !errors.Is(err, ErrUnsupportedCompressionAlgorithm) {
				t.Fatalf("unexpected error class %T (%v): every failure must be one of the two sentinels so the storage layer can classify it", err, err)
			}
			if out != nil {
				t.Fatalf("output must be nil on error, got %d bytes", len(out))
			}
			return
		}

		if len(out) > MaxDecompressedBytes {
			t.Fatalf("output of %d bytes exceeds MaxDecompressedBytes (%d)", len(out), MaxDecompressedBytes)
		}

		switch kind {
		case unframed:
			// Legacy passthrough: must be returned verbatim and byte-identical.
			if !bytes.Equal(out, original) {
				t.Fatalf("unframed data must be returned verbatim:\n got % x\nwant % x", out, original)
			}
			if len(original) > 0 && original[0] == frameDiscriminator {
				t.Fatalf("data beginning with the discriminator must never be classified as unframed: % x", original[:min(len(original), 8)])
			}

		case framedStored:
			if !bytes.Equal(out, original[headerSize:]) {
				t.Fatal("a stored frame must return its payload verbatim")
			}

		case framedDeflate:
			// Success implies the output is exactly the declared length.
			declared, w := binary.Uvarint(original[headerSize:])
			if w <= 0 {
				t.Fatal("a successful deflate frame must have a parseable declared length")
			}
			if uint64(len(out)) != declared {
				t.Fatalf("output length %d does not match the declared length %d", len(out), declared)
			}

		default:
			t.Fatalf("unknown frame kind %v", kind)
		}
	})
}

// FuzzTransformerRoundTrip fuzzes the write side, asserting the property that
// actually protects data: whatever the writer produces, the reader recovers the
// original bytes exactly.
func FuzzTransformerRoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("small"))
	f.Add([]byte{0x00, 0x6b, 0x38, 0x73, 0x01})             // looks like a frame
	f.Add(bytes.Repeat([]byte("compressible"), 200))        // above the threshold
	f.Add([]byte{0x6b, 0x38, 0x73, 0x00, 0x0a, 0x01, 0x41}) // protobuf-shaped

	f.Fuzz(func(t *testing.T, data []byte) {
		policy, err := NewPolicy(config(entry(deflate, "configmaps")))
		if err != nil {
			t.Fatal(err)
		}

		for _, p := range []*Policy{policy, nil} {
			tr := WithCompression(newRecording(), p, testGR)

			stored, err := tr.TransformToStorage(context.Background(), data, testDataCtx)
			if err != nil {
				t.Fatalf("write must never fail for any plaintext: %v", err)
			}
			out, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx)
			if err != nil {
				t.Fatalf("a value this apiserver just wrote must be readable: %v", err)
			}
			if !bytes.Equal(out, data) {
				t.Fatalf("round trip lost data:\n got % x\nwant % x",
					out[:min(len(out), 32)], data[:min(len(data), 32)])
			}
		}
	})
}
