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
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

func resetMetrics() {
	operationsTotal.Reset()
	formatErrorsTotal.Reset()
	unsupportedAlgorithmTotal.Reset()
	rewritesNeededTotal.Reset()
}

func TestOperationMetrics(t *testing.T) {
	RegisterMetrics()

	cases := []struct {
		desc    string
		run     func(t *testing.T)
		metrics []string
		want    string
	}{
		{
			desc: "compressed write",
			run: func(t *testing.T) {
				tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
				if _, err := tr.TransformToStorage(context.Background(), compressible, testDataCtx); err != nil {
					t.Fatal(err)
				}
			},
			metrics: []string{"apiserver_storage_compression_operations_total"},
			want: `
			# HELP apiserver_storage_compression_operations_total [ALPHA] Total number of storage compression operations, broken down by resource, operation and outcome. The outcome field distinguishes values that were compressed from values that were stored verbatim and from values that were not framed at all, so an operator can tell whether the size threshold or the per-resource policy is the reason a resource is not being compressed. Nothing is recorded for resources the policy does not select, unless a frame is encountered on read.
			# TYPE apiserver_storage_compression_operations_total counter
			apiserver_storage_compression_operations_total{operation="to_storage",outcome="deflate",resource="configmaps"} 1
			`,
		},
		{
			desc: "incompressible write is recorded as stored",
			run: func(t *testing.T) {
				tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
				if _, err := tr.TransformToStorage(context.Background(), randomBytes(4096), testDataCtx); err != nil {
					t.Fatal(err)
				}
			},
			metrics: []string{"apiserver_storage_compression_operations_total"},
			want: `
			# HELP apiserver_storage_compression_operations_total [ALPHA] Total number of storage compression operations, broken down by resource, operation and outcome. The outcome field distinguishes values that were compressed from values that were stored verbatim and from values that were not framed at all, so an operator can tell whether the size threshold or the per-resource policy is the reason a resource is not being compressed. Nothing is recorded for resources the policy does not select, unless a frame is encountered on read.
			# TYPE apiserver_storage_compression_operations_total counter
			apiserver_storage_compression_operations_total{operation="to_storage",outcome="stored",resource="configmaps"} 1
			`,
		},
		{
			desc: "below threshold write",
			run: func(t *testing.T) {
				tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
				if _, err := tr.TransformToStorage(context.Background(), []byte("{}"), testDataCtx); err != nil {
					t.Fatal(err)
				}
			},
			metrics: []string{"apiserver_storage_compression_operations_total"},
			want: `
			# HELP apiserver_storage_compression_operations_total [ALPHA] Total number of storage compression operations, broken down by resource, operation and outcome. The outcome field distinguishes values that were compressed from values that were stored verbatim and from values that were not framed at all, so an operator can tell whether the size threshold or the per-resource policy is the reason a resource is not being compressed. Nothing is recorded for resources the policy does not select, unless a frame is encountered on read.
			# TYPE apiserver_storage_compression_operations_total counter
			apiserver_storage_compression_operations_total{operation="to_storage",outcome="unframed_below_threshold",resource="configmaps"} 1
			`,
		},
		{
			desc: "above ceiling write",
			run: func(t *testing.T) {
				tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
				if _, err := tr.TransformToStorage(context.Background(), bytes.Repeat([]byte("a"), MaxPlaintextBytes+1), testDataCtx); err != nil {
					t.Fatal(err)
				}
			},
			metrics: []string{"apiserver_storage_compression_operations_total"},
			want: `
			# HELP apiserver_storage_compression_operations_total [ALPHA] Total number of storage compression operations, broken down by resource, operation and outcome. The outcome field distinguishes values that were compressed from values that were stored verbatim and from values that were not framed at all, so an operator can tell whether the size threshold or the per-resource policy is the reason a resource is not being compressed. Nothing is recorded for resources the policy does not select, unless a frame is encountered on read.
			# TYPE apiserver_storage_compression_operations_total counter
			apiserver_storage_compression_operations_total{operation="to_storage",outcome="unframed_above_max",resource="configmaps"} 1
			`,
		},
		{
			desc: "reading a framed value is recorded even with the policy off",
			run: func(t *testing.T) {
				tr := WithCompression(newRecording(), nil, testGR)
				stored := mustDeflate(t, compressible)
				if _, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx); err != nil {
					t.Fatal(err)
				}
			},
			metrics: []string{
				"apiserver_storage_compression_operations_total",
				"apiserver_storage_compression_rewrites_needed_total",
			},
			want: `
			# HELP apiserver_storage_compression_operations_total [ALPHA] Total number of storage compression operations, broken down by resource, operation and outcome. The outcome field distinguishes values that were compressed from values that were stored verbatim and from values that were not framed at all, so an operator can tell whether the size threshold or the per-resource policy is the reason a resource is not being compressed. Nothing is recorded for resources the policy does not select, unless a frame is encountered on read.
			# TYPE apiserver_storage_compression_operations_total counter
			apiserver_storage_compression_operations_total{operation="from_storage",outcome="deflate",resource="configmaps"} 1
			# HELP apiserver_storage_compression_rewrites_needed_total [ALPHA] Total number of reads that reported a value as stale because its on-disk compression framing disagreed with the current policy, and which will therefore be rewritten by the next update. Bounds and attributes the write amplification of enabling or disabling compression for a resource, and goes quiet once a storage version migration for that resource has converged.
			# TYPE apiserver_storage_compression_rewrites_needed_total counter
			apiserver_storage_compression_rewrites_needed_total{direction="to_unframed",resource="configmaps"} 1
			`,
		},
		{
			desc: "reading legacy data with the policy on drives migration",
			run: func(t *testing.T) {
				tr := WithCompression(newRecording(), policyFor(t, "configmaps"), testGR)
				if _, _, err := tr.TransformFromStorage(context.Background(), compressible, testDataCtx); err != nil {
					t.Fatal(err)
				}
			},
			metrics: []string{
				"apiserver_storage_compression_operations_total",
				"apiserver_storage_compression_rewrites_needed_total",
			},
			want: `
			# HELP apiserver_storage_compression_operations_total [ALPHA] Total number of storage compression operations, broken down by resource, operation and outcome. The outcome field distinguishes values that were compressed from values that were stored verbatim and from values that were not framed at all, so an operator can tell whether the size threshold or the per-resource policy is the reason a resource is not being compressed. Nothing is recorded for resources the policy does not select, unless a frame is encountered on read.
			# TYPE apiserver_storage_compression_operations_total counter
			apiserver_storage_compression_operations_total{operation="from_storage",outcome="unframed_legacy",resource="configmaps"} 1
			# HELP apiserver_storage_compression_rewrites_needed_total [ALPHA] Total number of reads that reported a value as stale because its on-disk compression framing disagreed with the current policy, and which will therefore be rewritten by the next update. Bounds and attributes the write amplification of enabling or disabling compression for a resource, and goes quiet once a storage version migration for that resource has converged.
			# TYPE apiserver_storage_compression_rewrites_needed_total counter
			apiserver_storage_compression_rewrites_needed_total{direction="to_framed",resource="configmaps"} 1
			`,
		},
		{
			desc: "unsupported algorithm is attributed to the resource",
			run: func(t *testing.T) {
				tr := WithCompression(newRecording(), nil, testGR)
				if _, _, err := tr.TransformFromStorage(context.Background(), []byte{0x00, 0x6b, 0x38, 0x73, 0xfe}, testDataCtx); err == nil {
					t.Fatal("expected an error")
				}
			},
			metrics: []string{"apiserver_storage_compression_unsupported_algorithm_total"},
			want: `
			# HELP apiserver_storage_compression_unsupported_algorithm_total [ALPHA] Total number of values encountered on read whose storage compression algorithm this apiserver does not implement, meaning a newer apiserver wrote them. This is version skew, not corruption: such an object is never eligible for unsafe deletion. A non-zero value means this apiserver is older than a peer and should be upgraded.
			# TYPE apiserver_storage_compression_unsupported_algorithm_total counter
			apiserver_storage_compression_unsupported_algorithm_total{resource="configmaps"} 1
			`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			resetMetrics()
			defer resetMetrics()
			tc.run(t)
			if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(tc.want), tc.metrics...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestNoMetricsWhenInert is the observability half of default inertness: a cluster
// that never enables compression must not even see its counters move, so that
// "nothing is happening" is unambiguous.
func TestNoMetricsWhenInert(t *testing.T) {
	RegisterMetrics()
	resetMetrics()
	defer resetMetrics()

	tr := WithCompression(newRecording(), nil, testGR)
	stored, err := tr.TransformToStorage(context.Background(), compressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx); err != nil {
		t.Fatal(err)
	}

	// Nothing at all should be gathered for these series.
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(""),
		"apiserver_storage_compression_operations_total",
		"apiserver_storage_compression_rewrites_needed_total",
		"apiserver_storage_compression_unsupported_algorithm_total",
	); err != nil {
		t.Errorf("inert policy produced metric samples: %v", err)
	}
}

// TestFormatErrorMetricCarriesResourceButNotReason pins the security decision
// that frame-parse failures are attributed to a resource but never to a reason.
// The resource tells an operator where to look; the reason -- bad magic, invalid
// declared length, stream shorter or longer than declared -- would tell an actor
// who can tamper with etcd which validation step their candidate ciphertext
// failed, which is a chosen-ciphertext format oracle against the unauthenticated
// providers. That detail is logged at V(4) instead.
//
// It also pins that a child series appears only once an error has actually been
// counted, so a healthy cluster exports no series for this counter at all.
func TestFormatErrorMetricCarriesResourceButNotReason(t *testing.T) {
	RegisterMetrics()
	formatErrorsTotal.Reset()

	const name = "apiserver_storage_compression_format_errors_total"

	find := func() []*dto.Metric {
		t.Helper()
		families, err := legacyregistry.DefaultGatherer.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() == name {
				return family.GetMetric()
			}
		}
		return nil
	}

	if got := find(); len(got) != 0 {
		t.Fatalf("%s exported %d series before any error was recorded; it must not be pre-initialized, so that a healthy cluster carries no series", name, len(got))
	}

	recordFormatError("configmaps")

	series := find()
	if len(series) != 1 {
		t.Fatalf("expected exactly 1 series after recording one error, got %d", len(series))
	}

	var labels []string
	for _, l := range series[0].GetLabel() {
		labels = append(labels, l.GetName())
	}
	if len(labels) != 1 || labels[0] != "resource" {
		t.Fatalf("%s must carry exactly the resource label, got %v: any label distinguishing why a frame was rejected is an adaptive format oracle under AES-CBC and the identity provider", name, labels)
	}
	if v := series[0].GetLabel()[0].GetValue(); v != "configmaps" {
		t.Fatalf("resource label = %q, want %q", v, "configmaps")
	}

	// A second resource must create a second series rather than pooling.
	recordFormatError("secrets")
	if got := find(); len(got) != 2 {
		t.Fatalf("expected 2 series after a second resource errored, got %d", len(got))
	}
}
