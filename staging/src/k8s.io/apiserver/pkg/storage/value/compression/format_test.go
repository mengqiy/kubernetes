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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"testing"
)

// TestFrameByteExact pins the literal on-disk header bytes. This is the
// equivalent of TestGCMDataStable for the AES transformer: it is what keeps the
// at-rest format from drifting silently, which would strand data written by an
// earlier release.
func TestFrameByteExact(t *testing.T) {
	t.Run("stored", func(t *testing.T) {
		got := storedFrame([]byte("hello"))
		want := []byte{0x00, 0x6b, 0x38, 0x73, 0x00, 'h', 'e', 'l', 'l', 'o'}
		if !bytes.Equal(got, want) {
			t.Fatalf("stored frame changed:\n got % x\nwant % x", got, want)
		}
	})

	t.Run("deflate header", func(t *testing.T) {
		// 4096 bytes encodes as the uvarint 0x80 0x20.
		got, err := deflateFrame(bytes.Repeat([]byte("a"), 4096))
		if err != nil {
			t.Fatal(err)
		}
		wantHeader := []byte{0x00, 0x6b, 0x38, 0x73, 0x01, 0x80, 0x20}
		if !bytes.HasPrefix(got, wantHeader) {
			t.Fatalf("deflate frame header changed:\n got % x\nwant prefix % x", got[:min(len(got), 16)], wantHeader)
		}
		if len(got) >= 4096 {
			t.Errorf("4096 repeated bytes should compress, got %d bytes", len(got))
		}
	})

	t.Run("magic constants", func(t *testing.T) {
		// Guard the constants themselves, since every other test derives from them.
		if frameDiscriminator != 0x00 {
			t.Errorf("frameDiscriminator must be 0x00, got %#x", frameDiscriminator)
		}
		if frameMagic != [4]byte{0x00, 'k', '8', 's'} {
			t.Errorf("frameMagic must be 00 6b 38 73, got % x", frameMagic)
		}
		if headerSize != 5 {
			t.Errorf("headerSize must be 5, got %d", headerSize)
		}
		if algStored != 0x00 || algDeflate != 0x01 {
			t.Errorf("algorithm ids changed: stored=%#x deflate=%#x", algStored, algDeflate)
		}
		if MaxDecompressedBytes != MaxPlaintextBytes {
			t.Errorf("MaxDecompressedBytes (%d) must equal MaxPlaintextBytes (%d): a value that decodes must always be one this apiserver could have written",
				MaxDecompressedBytes, MaxPlaintextBytes)
		}
	})
}

// TestRoundTrip walks the length matrix used by the AES and secretbox
// transformer tests, across compressible, realistic and incompressible payloads.
func TestRoundTrip(t *testing.T) {
	lengths := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17,
		128, 1023, 1024, 1025, 4096, 65536, 1 << 20}

	kinds := map[string]func(n int) []byte{
		"compressible":   func(n int) []byte { return bytes.Repeat([]byte("abcd"), n/4+1)[:n] },
		"incompressible": randomBytes,
		"protobuf-like": func(n int) []byte {
			// Starts with the real protobuf storage magic, so this also exercises
			// the case where the payload's own lead byte is meaningful.
			b := append([]byte{0x6b, 0x38, 0x73, 0x00}, bytes.Repeat([]byte("metadata"), n)...)
			return b[:max(n, 4)]
		},
	}

	for kindName, gen := range kinds {
		for _, n := range lengths {
			t.Run(fmt.Sprintf("%s/%d", kindName, n), func(t *testing.T) {
				plaintext := gen(n)

				for _, frame := range []struct {
					name string
					make func([]byte) ([]byte, error)
				}{
					{"stored", func(b []byte) ([]byte, error) { return storedFrame(b), nil }},
					{"deflate", deflateFrame},
				} {
					framed, err := frame.make(plaintext)
					if err != nil {
						t.Fatalf("%s: %v", frame.name, err)
					}
					if len(plaintext) == 0 && frame.name == "deflate" {
						// A zero-length plaintext is never framed by the writer
						// (it is below the threshold), and a declared length of
						// zero is rejected on read by design. Assert that
						// rejection rather than a round trip.
						if _, _, err := unframe(context.Background(), framed); !errors.Is(err, ErrCompressionFormat) {
							t.Fatalf("deflate frame declaring length 0 must be rejected, got %v", err)
						}
						continue
					}

					out, kind, err := unframe(context.Background(), framed)
					if err != nil {
						t.Fatalf("%s: unframe: %v", frame.name, err)
					}
					if !bytes.Equal(out, plaintext) {
						t.Fatalf("%s: round trip mismatch: got %d bytes, want %d", frame.name, len(out), len(plaintext))
					}
					wantKind := framedStored
					if frame.name == "deflate" {
						wantKind = framedDeflate
					}
					if kind != wantKind {
						t.Fatalf("%s: kind = %v, want %v", frame.name, kind, wantKind)
					}
				}
			})
		}
	}
}

// TestLegacyPassthrough is the backward-compatibility test: every byte sequence a
// kube-apiserver storage serializer can produce must be returned verbatim, with
// kind unframed and no error, without any configuration.
func TestLegacyPassthrough(t *testing.T) {
	cases := map[string][]byte{
		"protobuf":        append([]byte{0x6b, 0x38, 0x73, 0x00}, []byte("\n\x0fexample payload")...),
		"json":            []byte(`{"apiVersion":"v1","kind":"Pod"}`),
		"yaml":            []byte("apiVersion: v1\nkind: Pod\n"),
		"cbor":            append([]byte{0xd9, 0xd9, 0xf7}, 0xa1, 0x61, 'a', 0x01),
		"base64-protobuf": []byte("azhzAAoPZXhhbXBsZSBwYXlsb2Fk"),
		"empty":           {},
		"encrypted":       []byte("k8s:enc:aescbc:v1:key1:" + "\x01\x02\x03"),
		"single byte":     {0x7b},
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			// Copy so we can prove the input was not mutated.
			in := append([]byte(nil), data...)
			out, kind, err := unframe(context.Background(), in)
			if err != nil {
				t.Fatalf("legacy data must never error, got %v", err)
			}
			if kind != unframed {
				t.Fatalf("kind = %v, want unframed", kind)
			}
			if !bytes.Equal(out, data) {
				t.Fatalf("legacy data was modified:\n got % x\nwant % x", out, data)
			}
		})
	}
}

// TestUnframeRejections covers every reader rejection branch. Note that all
// malformed-frame cases must return the single ErrCompressionFormat sentinel and
// must NOT be distinguishable by error identity, because a distinguishable
// taxonomy would be a chosen-ciphertext format oracle under the unauthenticated
// providers. Only an unknown algorithm gets its own sentinel, because it means
// version skew and must never be treated as corruption.
func TestUnframeRejections(t *testing.T) {
	withDeclaredLen := func(n uint64, payload []byte) []byte {
		var hdr [headerSize + binary.MaxVarintLen64]byte
		copy(hdr[:], frameMagic[:])
		hdr[4] = algDeflate
		w := binary.PutUvarint(hdr[headerSize:], n)
		return append(append([]byte(nil), hdr[:headerSize+w]...), payload...)
	}
	deflateOf := func(b []byte) []byte {
		var buf bytes.Buffer
		zw, err := flate.NewWriter(&buf, deflateLevel)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := zw.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"discriminator only", []byte{0x00}, ErrCompressionFormat},
		{"truncated magic", []byte{0x00, 0x6b, 0x38}, ErrCompressionFormat},
		{"wrong magic", []byte{0x00, 'x', '8', 's', algDeflate, 0x01}, ErrCompressionFormat},
		{"header but no algorithm", []byte{0x00, 0x6b, 0x38, 0x73}, ErrCompressionFormat},
		{"unknown algorithm", []byte{0x00, 0x6b, 0x38, 0x73, 0xff, 0x01, 0x02}, ErrUnsupportedCompressionAlgorithm},
		{"unknown algorithm 0x02 reserved for zstd", []byte{0x00, 0x6b, 0x38, 0x73, 0x02}, ErrUnsupportedCompressionAlgorithm},
		{"declared length zero", withDeclaredLen(0, deflateOf(nil)), ErrCompressionFormat},
		{"declared length above cap", withDeclaredLen(MaxDecompressedBytes+1, deflateOf([]byte("x"))), ErrCompressionFormat},
		{"declared length absurd", withDeclaredLen(1<<40, deflateOf([]byte("x"))), ErrCompressionFormat},
		{"truncated varint", []byte{0x00, 0x6b, 0x38, 0x73, algDeflate, 0x80}, ErrCompressionFormat},
		{"no deflate stream", withDeclaredLen(16, nil), ErrCompressionFormat},
		{"corrupt deflate stream", withDeclaredLen(16, []byte{0xff, 0xff, 0xff, 0xff}), ErrCompressionFormat},
		{"stream shorter than declared", withDeclaredLen(1024, deflateOf(bytes.Repeat([]byte("a"), 512))), ErrCompressionFormat},
		{"stream longer than declared", withDeclaredLen(512, deflateOf(bytes.Repeat([]byte("a"), 1024))), ErrCompressionFormat},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := unframe(context.Background(), tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.want)
			}
			if out != nil {
				t.Errorf("output must be nil on error, got %d bytes", len(out))
			}
			// The two sentinels must remain distinguishable from each other, and
			// a malformed frame must never be reported as skew.
			if tc.want == ErrCompressionFormat && errors.Is(err, ErrUnsupportedCompressionAlgorithm) {
				t.Error("a malformed frame must not be reported as unsupported algorithm")
			}
			if tc.want == ErrUnsupportedCompressionAlgorithm && errors.Is(err, ErrCompressionFormat) {
				t.Error("version skew must not be reported as a malformed frame")
			}
		})
	}
}

// TestDeclaredLengthCheckedBeforeAllocation proves defence (1): an over-limit
// declared length is rejected without allocating the buffer it claims. Without
// this, a ~40 byte etcd write would be able to OOM every apiserver in the fleet
// on the next LIST or watch.
func TestDeclaredLengthCheckedBeforeAllocation(t *testing.T) {
	var hdr [headerSize + binary.MaxVarintLen64]byte
	copy(hdr[:], frameMagic[:])
	hdr[4] = algDeflate
	w := binary.PutUvarint(hdr[headerSize:], 1<<30) // claim 1 GiB
	bomb := append(append([]byte(nil), hdr[:headerSize+w]...), 0x01, 0x02, 0x03)

	const iterations = 200

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < iterations; i++ {
		if _, _, err := unframe(context.Background(), bomb); !errors.Is(err, ErrCompressionFormat) {
			t.Fatalf("bomb must be rejected, got %v", err)
		}
	}
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	// Each rejection allocates only a small error value. If the declared length
	// were honoured before the check, this would be iterations * 1 GiB.
	if allocated > 256*1024 {
		t.Fatalf("rejecting %d bombs allocated %d bytes; the declared length must be checked before allocation", iterations, allocated)
	}
}

// TestDecompressionBombBounded proves defences (2) and (3): a frame whose stream
// genuinely inflates far past its declared length is stopped, and the memory
// consumed is bounded by the declared length rather than by the expansion.
func TestDecompressionBombBounded(t *testing.T) {
	// 8 MiB of zeros compresses to a few KiB, but we declare only 1 KiB.
	huge := make([]byte, 8<<20)
	var buf bytes.Buffer
	zw, err := flate.NewWriter(&buf, deflateLevel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(huge); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 64<<10 {
		t.Fatalf("test precondition: expected a small compressed payload, got %d bytes", buf.Len())
	}

	var hdr [headerSize + binary.MaxVarintLen64]byte
	copy(hdr[:], frameMagic[:])
	hdr[4] = algDeflate
	w := binary.PutUvarint(hdr[headerSize:], 1024)
	bomb := append(append([]byte(nil), hdr[:headerSize+w]...), buf.Bytes()...)

	const iterations = 50

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < iterations; i++ {
		if _, _, err := unframe(context.Background(), bomb); !errors.Is(err, ErrCompressionFormat) {
			t.Fatalf("a stream longer than its declared length must be rejected, got %v", err)
		}
	}
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	// Per iteration we allocate the declared 1 KiB plus inflater state, and read
	// at most one byte past the declaration. We must NOT inflate all 8 MiB.
	if perIteration := allocated / iterations; perIteration > 1<<20 {
		t.Fatalf("allocated %d bytes per rejection; the stream must be bounded by the declared length, not drained", perIteration)
	}
}

// TestTrailingBytesIgnored pins the one respect in which the format is not
// canonical. Bytes appended after a complete DEFLATE stream are ignored, because
// flate stops at its final block and never reads them. This is documented rather
// than fixed: the decoded plaintext is exactly the declared length and exactly
// correct, the writer never emits trailing bytes, and anyone able to append them
// already has etcd write access. If this behaviour ever changes to a rejection,
// that is a compatible tightening, but it must be a deliberate decision.
func TestTrailingBytesIgnored(t *testing.T) {
	plaintext := bytes.Repeat([]byte("a"), 16)
	framed, err := deflateFrame(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	framed = append(framed, 0x99, 0x99)

	out, kind, err := unframe(context.Background(), framed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != framedDeflate {
		t.Errorf("kind = %v, want framedDeflate", kind)
	}
	if !bytes.Equal(out, plaintext) {
		t.Fatalf("plaintext must still be recovered exactly: got %q", out)
	}
}

// TestMaxDeclaredLengthAccepted checks the boundary: a frame declaring exactly
// MaxDecompressedBytes is allowed to allocate, because that is a value this
// apiserver could legitimately have written.
func TestMaxDeclaredLengthAccepted(t *testing.T) {
	plaintext := bytes.Repeat([]byte("kubernetes"), MaxPlaintextBytes/10+1)[:MaxPlaintextBytes]
	framed, err := deflateFrame(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	out, kind, err := unframe(context.Background(), framed)
	if err != nil {
		t.Fatalf("a frame at exactly MaxPlaintextBytes must round trip, got %v", err)
	}
	if kind != framedDeflate || !bytes.Equal(out, plaintext) {
		t.Fatal("round trip at the size ceiling failed")
	}
}

// TestInflateBudgetRespectsContext proves that acquiring the inflation budget
// honours request cancellation rather than blocking forever or failing fast.
func TestInflateBudgetRespectsContext(t *testing.T) {
	// Saturate the budget.
	held := 0
	for {
		select {
		case inflateBudget <- struct{}{}:
			held++
			continue
		default:
		}
		break
	}
	defer func() {
		for i := 0; i < held; i++ {
			<-inflateBudget
		}
	}()

	framed, err := deflateFrame(bytes.Repeat([]byte("a"), 2048))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := unframe(ctx, framed); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	} else if errors.Is(err, ErrCompressionFormat) || errors.Is(err, ErrUnsupportedCompressionAlgorithm) {
		t.Error("cancellation must not be reported as a format or skew error: it is neither corruption nor a stored-format problem")
	}
}

// TestConcurrentFraming exercises the pooled writer and reader state under -race.
// Required because watch decode runs TransformFromStorage on multiple goroutines
// per watch stream and every LIST item calls it on the request goroutine.
func TestConcurrentFraming(t *testing.T) {
	const goroutines = 16
	const iterations = 50

	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			for i := 0; i < iterations; i++ {
				plaintext := bytes.Repeat([]byte(fmt.Sprintf("goroutine-%d-iteration-%d-", g, i)), 64)
				framed, err := deflateFrame(plaintext)
				if err != nil {
					errCh <- err
					return
				}
				out, _, err := unframe(context.Background(), framed)
				if err != nil {
					errCh <- err
					return
				}
				if !bytes.Equal(out, plaintext) {
					errCh <- fmt.Errorf("goroutine %d iteration %d: pooled coder state leaked between calls", g, i)
					return
				}
			}
			errCh <- nil
		}(g)
	}
	for g := 0; g < goroutines; g++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

// TestUnframeDoesNotAliasInput guards against a reader that hands back a slice of
// its input for the compressed case, which would let a later mutation of the
// etcd buffer corrupt an already-decoded object.
func TestUnframeDoesNotAliasInput(t *testing.T) {
	plaintext := bytes.Repeat([]byte("payload"), 512)
	framed, err := deflateFrame(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := unframe(context.Background(), framed)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 0 && len(framed) > 0 && &out[0] == &framed[0] {
		t.Fatal("decompressed output aliases the frame")
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return b
}
