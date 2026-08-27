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
	"context"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/klog/v2"
)

func init() {
	RegisterMetrics()
}

// transformer compresses the serialized object before the delegate encrypts it,
// and decompresses after the delegate decrypts.
//
// The ordering falls out of the decorator shape: TransformToStorage frames and
// *then* calls the delegate, so the effective pipeline is
// compress -> encrypt -> prefix. TransformFromStorage calls the delegate first,
// so it is prefix-match -> decrypt -> decompress.
type transformer struct {
	delegate value.Transformer
	// resource is the pre-computed metric label for groupResource.
	resource string
	// enabled is the write policy for this resource, resolved once at
	// construction. That is correct here because the policy comes from flags,
	// which require a restart; it is never consulted on the read path.
	enabled bool
	// expansion holds math.Float64bits of an EWMA of plaintext-bytes over
	// stored-bytes, sampled on reads. See ExpansionFactor.
	expansion atomic.Uint64
}

var (
	_ value.Transformer     = &transformer{}
	_ PlaintextSizeExpander = &transformer{}
)

// PlaintextSizeExpander is implemented by a transformer whose stored bytes may be
// smaller than the plaintext they represent, so that a caller measuring stored
// bytes can correct its estimate of in-memory cost.
//
// This exists because the resource size estimator records len(kv.Value) -- the
// stored, possibly compressed size -- and that value flows into API Priority and
// Fairness, where it is used to bound the memory a LIST will occupy. The memory
// APF is protecting against is the decompressed, decoded size, so without a
// correction APF would admit ratio-times more concurrent LISTs than the apiserver
// can serve.
type PlaintextSizeExpander interface {
	// ExpansionFactor returns a multiplier >= 1.0 to apply to a stored-byte
	// measurement to estimate the corresponding plaintext size.
	ExpansionFactor() float64
}

// expansionEWMAAlpha is the smoothing factor for the expansion estimate. 1/64
// converges within a few hundred reads while staying insensitive to a single
// unusual object.
const expansionEWMAAlpha = 1.0 / 64.0

// ExpansionFactor implements PlaintextSizeExpander.
//
// The estimate is an EWMA sampled on the READ path, over every value read
// including uncompressed ones. Sampling reads rather than writes is what makes it
// track the population actually in storage: while a resource is still mostly
// legacy-uncompressed, reads overwhelmingly yield a ratio of 1.0 and the factor
// stays near 1.0, rising only as migration progresses. A write-side estimate
// would instead jump to the full compression ratio on the first compressed write,
// while almost all stored bytes were still uncompressed, and over-charge by that
// ratio; a fixed pessimistic constant would over-charge permanently on any
// resource that compresses poorly.
//
// It is an estimate feeding an estimate, and it is deliberately floored at 1.0 so
// it can only ever make APF more conservative than measuring stored bytes alone.
func (t *transformer) ExpansionFactor() float64 {
	f := math.Float64frombits(t.expansion.Load())
	if f < 1 || math.IsNaN(f) {
		return 1
	}
	return f
}

func (t *transformer) observeExpansion(plaintextLen, storedLen int) {
	if storedLen <= 0 || plaintextLen < 0 {
		return
	}
	sample := float64(plaintextLen) / float64(storedLen)
	if sample < 1 {
		// Framing overhead on an incompressible value can make stored slightly
		// larger than plaintext. Never let that pull the factor below 1.
		sample = 1
	}
	// A lost update here just means one dropped sample, which is immaterial for
	// an exponentially weighted average, so this deliberately avoids a CAS loop
	// on the read hot path.
	prev := math.Float64frombits(t.expansion.Load())
	if prev < 1 || math.IsNaN(prev) {
		prev = 1
	}
	t.expansion.Store(math.Float64bits(prev + expansionEWMAAlpha*(sample-prev)))
}

// WithCompression decorates delegate so that values of gr are compressed before
// delegate encrypts them.
//
// The caller MUST install this decorator unconditionally, whatever the policy
// says. Read support cannot be allowed to depend on configuration: a value
// framed by any apiserver has to be readable by every apiserver of the same or a
// newer version, with any flags, or else dropping a flag would strand data and
// the central safety property of the design would become its central bug.
// Default inertness is preserved instead by the policy check inside
// TransformToStorage and by unframe returning non-framed bytes verbatim.
func WithCompression(delegate value.Transformer, policy *Policy, gr schema.GroupResource) value.Transformer {
	t := &transformer{
		delegate: delegate,
		resource: gr.String(),
		enabled:  policy.EnabledFor(gr),
	}
	t.expansion.Store(math.Float64bits(1))
	return t
}

// willFrame is both the write predicate and the staleness predicate, and it takes
// the plaintext itself rather than just its length so that the two sites are
// literally the same function.
//
// That identity is what makes migration terminate. The writer emits a frame
// exactly when willFrame is true -- algDeflate if it shrinks the value, algStored
// if it does not -- and the reader reports stale exactly when the on-disk framing
// disagrees with willFrame. So one rewrite per object converges, in either
// direction, and nothing can ping-pong.
//
// The isFramed clause is not redundant. A plaintext that already begins with the
// frame discriminator is always escaped into a frame regardless of the policy (see
// TransformToStorage), so if the staleness predicate ignored that, such a value
// would be reported stale on every read with the policy off, rewritten into a
// frame, and reported stale again forever.
func (t *transformer) willFrame(plaintext []byte) bool {
	if isFramed(plaintext) {
		return true
	}
	return t.enabled && len(plaintext) >= MinPlaintextBytes && len(plaintext) <= MaxPlaintextBytes
}

func (t *transformer) TransformToStorage(ctx context.Context, data []byte, dataCtx value.Context) ([]byte, error) {
	// Escape, never reject. If a future storage serializer ever emitted a
	// leading 0x00 the frame would be ambiguous, so wrap it in a stored frame,
	// which round-trips it exactly. An earlier iteration of this design returned
	// an error here instead; that would mean a *disabled* feature could fail
	// every write of an affected resource cluster-wide, which is a hard outage
	// bought for nothing. Keeping the frame a total function costs 5 bytes in a
	// branch that TestNoStorageSerializerEmitsFrameDiscriminator asserts is
	// unreachable.
	if isFramed(data) {
		klog.V(2).InfoS("storage compression: plaintext begins with the frame discriminator; wrapping it verbatim to keep the at-rest format unambiguous", "resource", t.resource)
		recordOperation(t.resource, toStorage, outcomeStored)
		return t.delegate.TransformToStorage(ctx, storedFrame(data), dataCtx)
	}

	if !t.willFrame(data) {
		// Fully inert when the policy does not select this resource: no framing,
		// no metrics, no allocation.
		if t.enabled {
			recordOperation(t.resource, toStorage, skipOutcome(len(data)))
		}
		return t.delegate.TransformToStorage(ctx, data, dataCtx)
	}

	start := time.Now()
	framed, err := deflateFrame(data)
	if err != nil {
		// Compression itself failing is a bug, not hostile input. Fail the write
		// rather than silently storing an unframed value, which would be
		// reported stale on every subsequent read.
		return nil, err
	}
	recordDuration(toStorage, time.Since(start))

	// Never expand: if deflate did not beat storing the value verbatim inside a
	// frame, store it verbatim inside a frame. Note the comparison is against
	// the *stored frame* size rather than against the raw plaintext, because the
	// value must end up framed either way for the staleness rule to terminate.
	outcome := outcomeDeflate
	if len(framed) >= headerSize+len(data) {
		framed, outcome = storedFrame(data), outcomeStored
	}
	recordOperation(t.resource, toStorage, outcome)
	return t.delegate.TransformToStorage(ctx, framed, dataCtx)
}

func (t *transformer) TransformFromStorage(ctx context.Context, data []byte, dataCtx value.Context) ([]byte, bool, error) {
	plain, stale, err := t.delegate.TransformFromStorage(ctx, data, dataCtx)
	if err != nil {
		return nil, stale, err
	}

	start := time.Now()
	out, kind, err := unframe(ctx, plain)
	if err != nil {
		t.recordReadError(ctx, dataCtx, err)
		return nil, stale, err
	}

	// Sampled for every read, framed or not, so that the factor reflects the mix
	// actually in storage rather than only the values this apiserver compressed.
	t.observeExpansion(len(out), len(plain))

	switch kind {
	case unframed:
		// Legacy, or written while the policy did not select this resource.
		// Recorded only when the policy is on, so that a cluster which never
		// enables compression is byte-for-byte and metric-for-metric inert.
		if t.enabled {
			recordOperation(t.resource, fromStorage, outcomeUnframedLegacyRead)
		}
	case framedStored:
		recordOperation(t.resource, fromStorage, outcomeStored)
	case framedDeflate:
		recordDuration(fromStorage, time.Since(start))
		recordOperation(t.resource, fromStorage, outcomeDeflate)
	}

	return out, stale || t.markStale(kind, out), nil
}

// markStale reports whether the on-disk framing disagrees with what the current
// policy would produce, which defeats the no-op-write short circuit in
// GuaranteedUpdate and causes the next update -- or a storage version migration
// -- to rewrite the value in the right form.
func (t *transformer) markStale(kind frameKind, plaintext []byte) bool {
	framed := kind != unframed
	want := t.willFrame(plaintext)
	if framed == want {
		return false
	}
	if want {
		recordRewriteNeeded(t.resource, directionToFramed)
	} else {
		recordRewriteNeeded(t.resource, directionToUnframed)
	}
	return true
}

func (t *transformer) recordReadError(ctx context.Context, dataCtx value.Context, err error) {
	switch {
	case errors.Is(err, ErrUnsupportedCompressionAlgorithm):
		recordUnsupportedAlgorithm(t.resource)
	case errors.Is(err, ErrCompressionFormat):
		recordFormatError(t.resource)
	default:
		// Context cancellation while waiting for the inflation budget. Not a
		// data problem; the request's own error accounting covers it.
		return
	}

	// The reason and the storage key go to the log, never to a metric label.
	// Without this an SRE facing "object X is unreadable" has an unlabeled
	// counter and nothing to grep, and the watch path does not surface the key
	// at all. V(4) is the gating mechanism, following the precedent of
	// logTransformErr in the value package.
	if klogV := klog.V(4); klogV.Enabled() {
		var key string
		if dataCtx != nil {
			key = string(dataCtx.AuthenticatedData())
		}
		klogV.InfoS("failed to read storage compression frame", "err", err, "resource", t.resource, "key", key)
	}
}

// skipOutcome is only reached when the policy selects the resource, so the value
// was skipped because of its size.
func skipOutcome(plaintextLen int) string {
	if plaintextLen > MaxPlaintextBytes {
		return outcomeUnframedAboveMax
	}
	return outcomeUnframedBelowMin
}
