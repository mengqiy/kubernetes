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
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	namespace = "apiserver"
	subsystem = "storage"

	// operation label values, reusing the vocabulary of the existing
	// apiserver_storage_transformation_* metrics.
	toStorage   = "to_storage"
	fromStorage = "from_storage"

	// outcome label values.
	outcomeDeflate            = "deflate"
	outcomeStored             = "stored"
	outcomeUnframedBelowMin   = "unframed_below_threshold"
	outcomeUnframedAboveMax   = "unframed_above_max"
	outcomeUnframedLegacyRead = "unframed_legacy"

	// direction label values for rewritesNeededTotal.
	directionToFramed   = "to_framed"
	directionToUnframed = "to_unframed"
)

/*
 * All metrics in this file are ALPHA.
 *
 * Deliberately NOT added, for security rather than cardinality reasons: any
 * compression-ratio histogram, any plaintext/stored byte counter, and any
 * per-object or per-key size metric, gauge, log line at default verbosity,
 * trace attribute, or event.
 *
 * Those are sharper compressibility oracles than the plaintext-length channel
 * that compression-before-encryption opens in the first place. A per-resource
 * ratio histogram on a resource with a handful of objects is effectively
 * per-object compressibility, and differencing two consecutive scrapes of a byte
 * counter across a single induced write yields the exact plaintext and stored
 * size of one object -- available to anyone who can read /metrics, a far broader
 * population than "can read etcd backups".
 *
 * On a resource holding very few objects, the `outcome` label does make one
 * property of each write observable to a /metrics reader: whether that object
 * crossed the framing size threshold. That is accepted. It is not a new
 * capability, since object length is already exposed more coarsely by
 * apiserver_storage_size_bytes and the resource size estimate; because algStored
 * exists, the label reflects only the threshold and the 0x00 escape rather than
 * compressibility; and it is the minimum needed to make the feature operable at
 * all -- without it an operator cannot tell whether the threshold or the policy
 * is the reason a resource is not being compressed.
 */
var (
	operationsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "compression_operations_total",
			Help:           "Total number of storage compression operations, broken down by resource, operation and outcome. The outcome field distinguishes values that were compressed from values that were stored verbatim and from values that were not framed at all, so an operator can tell whether the size threshold or the per-resource policy is the reason a resource is not being compressed. Nothing is recorded for resources the policy does not select, unless a frame is encountered on read.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource", "operation", "outcome"},
	)

	durationSeconds = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "compression_duration_seconds",
			Help:      "Latencies in seconds of storage compression and decompression, excluding the encryption transformer beneath it. This is reported separately because compression time is otherwise invisible: the etcd latency tracker wraps only the clientv3 round trip, the store's tracing spans emit only above 500ms, and because this layer sits outside prefixTransformers its cost is not included in apiserver_storage_transformation_duration_seconds either.",
			// Compression of a 1 KiB..1.5 MiB value at level 1 runs on the order
			// of tens of microseconds to a few milliseconds.
			Buckets:        metrics.ExponentialBuckets(5e-6, 2, 20),
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation"},
	)

	// formatErrorsTotal carries the resource but deliberately NOT the reason a
	// frame was rejected. See ErrCompressionFormat: a labeled reason -- bad
	// magic, invalid declared length, stream shorter or longer than declared --
	// would be a chosen-ciphertext format oracle against the unauthenticated
	// providers, letting an actor who can tamper with etcd learn which
	// validation step failed by reading /metrics, without holding any key. The
	// resource carries no such signal. The distinguishing detail is logged at
	// V(4) instead, where an operator can see it and a /metrics reader cannot.
	//
	// Not pre-initialized: a child series appears only on first increment, so a
	// healthy cluster exports no series at all for this counter.
	formatErrorsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "compression_format_errors_total",
			Help:           "Total number of malformed storage compression frames encountered on read, by resource. The specific reason is deliberately not a label -- it would be a format oracle against unauthenticated providers -- and is logged at verbosity 4 along with the storage key instead. A non-zero value means either a bug in this apiserver or tampering with etcd, and is a signal to investigate rather than to roll back.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource"},
	)

	unsupportedAlgorithmTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "compression_unsupported_algorithm_total",
			Help:           "Total number of values encountered on read whose storage compression algorithm this apiserver does not implement, meaning a newer apiserver wrote them. This is version skew, not corruption: such an object is never eligible for unsafe deletion. A non-zero value means this apiserver is older than a peer and should be upgraded.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource"},
	)

	// rewritesNeededTotal counts reads that reported the value as stale because
	// its on-disk framing disagreed with the current policy. It is measured on
	// the read side on purpose: that is what makes it a migration-completion
	// signal, since it goes quiet once every object of the resource has
	// converged.
	rewritesNeededTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "compression_rewrites_needed_total",
			Help:           "Total number of reads that reported a value as stale because its on-disk compression framing disagreed with the current policy, and which will therefore be rewritten by the next update. Bounds and attributes the write amplification of enabling or disabling compression for a resource, and goes quiet once a storage version migration for that resource has converged.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"resource", "direction"},
	)
)

var registerMetrics sync.Once

// RegisterMetrics registers the storage compression metrics. It is idempotent.
func RegisterMetrics() {
	registerMetrics.Do(func() {
		legacyregistry.MustRegister(operationsTotal)
		legacyregistry.MustRegister(durationSeconds)
		legacyregistry.MustRegister(formatErrorsTotal)
		legacyregistry.MustRegister(unsupportedAlgorithmTotal)
		legacyregistry.MustRegister(rewritesNeededTotal)
	})
}

func recordOperation(resource, operation, outcome string) {
	operationsTotal.WithLabelValues(resource, operation, outcome).Inc()
}

func recordDuration(operation string, elapsed time.Duration) {
	durationSeconds.WithLabelValues(operation).Observe(elapsed.Seconds())
}

func recordFormatError(resource string) {
	formatErrorsTotal.WithLabelValues(resource).Inc()
}

func recordUnsupportedAlgorithm(resource string) {
	unsupportedAlgorithmTotal.WithLabelValues(resource).Inc()
}

func recordRewriteNeeded(resource, direction string) {
	rewritesNeededTotal.WithLabelValues(resource, direction).Inc()
}
