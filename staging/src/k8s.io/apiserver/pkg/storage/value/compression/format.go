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

// Package compression implements compression of the serialized form of an API
// object before it is handed to the encryption-at-rest transformer chain and
// written to etcd.
//
// Compression must happen before encryption: ciphertext is incompressible, so
// compress-then-encrypt is the only ordering that yields a useful ratio. This
// package is therefore composed as a value.Transformer decorator that wraps the
// encryption chain -- on write it frames the plaintext and hands the result to
// the encrypting transformer, and on read the encrypting transformer decrypts
// first and this package unframes the plaintext.
//
// Backward compatibility is a property of the bytes, not of configuration. A
// frame begins with 0x00, which no kube-apiserver storage serializer can emit,
// so pre-existing uncompressed values are recognized by a single byte
// comparison with no flag, probe, or per-resource state. Decompression is
// therefore unconditional and must never be disableable; only the write path is
// gated.
package compression

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// Frame layout. The values in this block are part of the at-rest format.
//
//	offset  size    value / meaning
//	------  ------  --------------------------------------------------------
//	0       1       0x00           discriminator; no storage serializer can
//	                               emit a leading 0x00
//	1       3       6b 38 73       "k8s"; structure check, and makes a
//	                               decrypted frame recognizable in `od` output
//	4       1       algorithm      algStored | algDeflate
//	--- algStored ---
//	5       n       plaintext verbatim.            Overhead: exactly 5 bytes.
//	--- algDeflate ---
//	5       w       declared plaintext length, unsigned LEB128 (1..3 bytes)
//	5+w     m       raw DEFLATE stream (RFC 1951), level 1.
//	                                               Overhead: 6..8 bytes.
//
// There is deliberately no separate version byte, flags byte, or reserved byte:
// the algorithm id is the version discriminator, since an unknown id already
// carries exactly the signal a reader needs ("a newer apiserver wrote this"),
// and a future format change simply claims a new id.
const (
	// frameDiscriminator is the first byte of every frame. Do not change: it is
	// the entire backward-compatibility mechanism. It is safe because no
	// kube-apiserver storage serializer can produce a leading 0x00 -- protobuf
	// begins 0x6b 0x38 0x73 0x00 (protoEncodingPrefix), JSON begins '{', CBOR
	// begins 0xd9 0xd9 0xf7 (selfDescribedCBOR), YAML forbids NUL in a
	// character stream, and the legacy base64 fallback has NUL outside its
	// alphabet. TestNoStorageSerializerEmitsFrameDiscriminator enforces this.
	frameDiscriminator byte = 0x00

	// algStored means the payload is the verbatim plaintext. It is written when
	// a value was evaluated for compression and deliberately kept as-is,
	// which is what distinguishes "legacy, never evaluated" from "evaluated,
	// no gain" and makes the staleness rule terminate in one rewrite.
	// Do not change: backward incompatible.
	algStored byte = 0x00

	// algDeflate is raw DEFLATE, RFC 1951, no zlib or gzip wrapper.
	// Do not change: backward incompatible.
	algDeflate byte = 0x01

	// headerSize is the size of the fixed part of the frame: magic + algorithm.
	// Do not change: backward incompatible.
	headerSize = 5

	// MinPlaintextBytes is the size below which a value is never framed.
	// Measured, sub-KiB protobuf objects compress only 1.25x-1.5x, which does
	// not pay for the CPU on every read and write. This mirrors the
	// defaultGzipThresholdBytes precedent for HTTP responses, scaled down
	// because etcd values are hard-capped near 1.5 MiB and a 128 KiB floor
	// would make the feature a near-no-op.
	MinPlaintextBytes = 1024

	// MaxPlaintextBytes is the size above which a value is never framed, and
	// simultaneously the read-side inflation cap.
	//
	// These are deliberately the SAME constant, and that identity is essential. If
	// the write path framed plaintext larger than etcd's per-value ceiling,
	// compression would create objects that can be read but never rewritten,
	// and that become unwritable the moment the feature is disabled or the
	// apiserver is downgraded. Refusing to frame above the ceiling makes such
	// objects fail on write exactly as they do today, and deriving the read cap
	// from the same constant guarantees that anything which decodes is
	// something this apiserver could have written -- which also sets the
	// decompression-bomb cap at the real value ceiling rather than at a
	// multiple above it.
	//
	// The value matches etcd's embed.DefaultMaxRequestBytes (1.5 MiB).
	MaxPlaintextBytes = 1536 * 1024

	// MaxDecompressedBytes bounds a single inflation. See MaxPlaintextBytes.
	MaxDecompressedBytes = MaxPlaintextBytes

	// deflateLevel follows the in-tree precedent for HTTP response compression
	// (gzipContentEncodingLevel = 1), which records that level 1 "uses least
	// CPU compared to higher levels, yet offers similar compression ratios".
	// Level 1 also uses Go's small deflatefast encoder rather than the full
	// compressor, which matters because the writer pool holds one per
	// concurrent write. Decoders are level-agnostic, so a fleet running mixed
	// levels interoperates.
	deflateLevel = 1
)

// frameMagic is the 4-byte prefix of every frame: the discriminator followed by
// "k8s". Do not change: backward incompatible.
var frameMagic = [4]byte{frameDiscriminator, 'k', '8', 's'}

// ErrUnsupportedCompressionAlgorithm indicates that a value carries a frame
// whose algorithm id this apiserver does not implement -- that is, a newer
// apiserver wrote it.
//
// This is version skew, NOT corruption, and it must never authorize deletion of
// the object. It is a sentinel compared with errors.Is rather than a typed error
// compared with errors.As, because prefixTransformers returns
// utilerrors.NewAggregate when more than one provider matches a prefix (during
// key rotation, for example), and that aggregate implements Is but not As.
var ErrUnsupportedCompressionAlgorithm = errors.New("unsupported storage compression algorithm")

// ErrCompressionFormat indicates a malformed frame. Every frame-parse failure
// returns this single sentinel and increments one unlabeled counter; the
// distinguishing detail goes only to a rate-limited log line.
//
// This is deliberate. The frame header is known plaintext and AES-CBC is
// unauthenticated (bare CBC with PKCS#7 and no MAC), so an actor able to tamper
// with etcd can set header bytes to chosen values without the key. Exposing a
// labeled reason on /metrics would turn the one bit that is observable today
// ("decodes / does not decode") into an adaptive chosen-ciphertext format
// oracle -- one created by the mitigation rather than by the compression.
var ErrCompressionFormat = errors.New("malformed storage compression frame")

// frameKind describes what was found on disk.
type frameKind int

const (
	// unframed is a value with no frame: legacy data, or a value written while
	// the policy did not select the resource.
	unframed frameKind = iota
	// framedStored is a frame with algStored.
	framedStored
	// framedDeflate is a frame with algDeflate.
	framedDeflate
)

var (
	writerPool = sync.Pool{
		New: func() any {
			w, err := flate.NewWriter(io.Discard, deflateLevel)
			if err != nil {
				// Only returned for an invalid level, which is a constant here.
				panic(fmt.Sprintf("compression: invalid deflate level %d: %v", deflateLevel, err))
			}
			return w
		},
	}

	readerPool = sync.Pool{
		New: func() any {
			return flate.NewReader(bytes.NewReader(nil))
		},
	}
)

// inflateBudget bounds the number of concurrent inflations process-wide, so the
// worst-case in-flight decompressed memory is a constant rather than something
// clients can drive.
//
// A per-call cap alone would not be enough: watch decode runs on up to
// processEventConcurrency goroutines *per watch stream*, so without a global
// bound the worst case would be MaxDecompressedBytes x concurrency x number of
// watch streams. Acquisition blocks on the caller's context rather than failing
// fast, so load becomes latency rather than errors.
var inflateBudget = make(chan struct{}, maxInflightInflations())

func maxInflightInflations() int {
	n := runtime.GOMAXPROCS(0) * 2
	if n < 4 {
		n = 4
	}
	if n > 64 {
		n = 64
	}
	return n
}

func acquireInflateBudget(ctx context.Context) (release func(), err error) {
	select {
	case inflateBudget <- struct{}{}:
		return func() { <-inflateBudget }, nil
	default:
	}
	// Slow path: contended. Wait, but honour request cancellation.
	select {
	case inflateBudget <- struct{}{}:
		return func() { <-inflateBudget }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// isFramed reports whether data carries a compression frame. This is the entire
// backward-compatibility test: one byte, no configuration.
func isFramed(data []byte) bool {
	return len(data) > 0 && data[0] == frameDiscriminator
}

// storedFrame wraps plaintext in a frame with algStored. Overhead is exactly
// headerSize bytes.
func storedFrame(plaintext []byte) []byte {
	out := make([]byte, 0, headerSize+len(plaintext))
	out = append(out, frameMagic[:]...)
	out = append(out, algStored)
	return append(out, plaintext...)
}

// deflateFrame compresses plaintext into a frame with algDeflate. The caller is
// responsible for deciding whether the result is worth keeping; deflateFrame
// makes no size judgement.
func deflateFrame(plaintext []byte) ([]byte, error) {
	var hdr [headerSize + binary.MaxVarintLen64]byte
	copy(hdr[:], frameMagic[:])
	hdr[4] = algDeflate
	w := binary.PutUvarint(hdr[headerSize:], uint64(len(plaintext)))

	// Guess an output size that avoids a realloc for typical API objects
	// without over-committing for incompressible ones.
	buf := bytes.NewBuffer(make([]byte, 0, headerSize+w+len(plaintext)/2+64))
	buf.Write(hdr[:headerSize+w])

	zw := writerPool.Get().(*flate.Writer)
	defer writerPool.Put(zw)
	zw.Reset(buf)
	if _, err := zw.Write(plaintext); err != nil {
		return nil, fmt.Errorf("compression: deflate write failed: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compression: deflate close failed: %w", err)
	}
	return buf.Bytes(), nil
}

// unframe reverses storedFrame and deflateFrame. A value without a frame is
// returned verbatim with kind unframed and no error: that is the
// backward-compatibility path, and it is the common case forever.
//
// unframe must treat its input as hostile. AES-CBC and the KMS v1 CBC read
// fallback are unauthenticated and malleable, and the identity provider does not
// authenticate at all, so an actor with etcd write access can steer these bytes.
// This is the first apiserver code path that allocates based on length metadata
// read from storage, so the bound is explicit rather than argued:
//
//  1. the declared length is checked against MaxDecompressedBytes before
//     anything is allocated;
//  2. there is exactly one right-sized allocation, never a growing buffer;
//  3. the stream is bounded independently of its own declaration in both
//     directions -- io.ReadFull rejects a stream that yields fewer bytes than
//     declared, and a 1-byte probe rejects one that yields more, so a lying
//     header is always an error and never a silent truncation;
//  4. concurrent inflations are capped process-wide.
//
// The format is deliberately not canonical in one respect: bytes appended after
// a complete DEFLATE stream are ignored, because flate stops at its final block
// and never reads them. Detecting that would require a second pass to find where
// the stream ends, and the payoff is nil -- the decoded plaintext is exactly the
// declared length and exactly correct either way, the writer never emits trailing
// bytes, and anyone able to append them already has etcd write access. What the
// probe does catch, and what actually matters, is a stream that inflates past its
// declared length.
func unframe(ctx context.Context, data []byte) ([]byte, frameKind, error) {
	if !isFramed(data) {
		return data, unframed, nil
	}
	if len(data) < headerSize || !bytes.Equal(data[:len(frameMagic)], frameMagic[:]) {
		return nil, unframed, fmt.Errorf("%w: bad magic", ErrCompressionFormat)
	}

	switch alg := data[4]; alg {
	case algStored:
		return data[headerSize:], framedStored, nil

	case algDeflate:
		n, w := binary.Uvarint(data[headerSize:])
		if w <= 0 || n == 0 || n > MaxDecompressedBytes {
			return nil, framedDeflate, fmt.Errorf("%w: invalid declared length", ErrCompressionFormat)
		}

		release, err := acquireInflateBudget(ctx)
		if err != nil {
			// Context cancellation, not a format problem. Do not map this onto
			// ErrCompressionFormat: it is neither corruption nor skew.
			return nil, framedDeflate, err
		}
		defer release()

		out := make([]byte, int(n))
		zr := readerPool.Get().(io.ReadCloser)
		defer readerPool.Put(zr)
		if err := zr.(flate.Resetter).Reset(bytes.NewReader(data[headerSize+w:]), nil); err != nil {
			return nil, framedDeflate, fmt.Errorf("%w: inflater reset failed", ErrCompressionFormat)
		}
		if _, err := io.ReadFull(zr, out); err != nil {
			return nil, framedDeflate, fmt.Errorf("%w: stream shorter than declared length", ErrCompressionFormat)
		}
		// The stream must end exactly here. Read at most one more byte so that
		// a stream which inflates far past its declared length costs one byte
		// of work rather than being drained.
		extra, err := io.Copy(io.Discard, io.LimitReader(zr, 1))
		if err != nil || extra != 0 {
			return nil, framedDeflate, fmt.Errorf("%w: stream longer than declared length", ErrCompressionFormat)
		}
		return out, framedDeflate, nil

	default:
		return nil, unframed, fmt.Errorf("%w: algorithm %#x", ErrUnsupportedCompressionAlgorithm, alg)
	}
}
