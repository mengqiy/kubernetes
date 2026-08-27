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
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/storage/value"
	aestransformer "k8s.io/apiserver/pkg/storage/value/encrypt/aes"
	"k8s.io/apiserver/pkg/storage/value/encrypt/identity"
)

var (
	testGR       = gr("", "configmaps")
	testDataCtx  = value.DefaultContext("/registry/configmaps/default/test")
	compressible = bytes.Repeat([]byte("kubernetes-api-object-"), 200) // 4400 bytes
)

// recordingTransformer stands in for the encryption chain. It records exactly the
// bytes handed to it on write, which is how the ordering assertion is made: if
// compression runs before encryption, these bytes are a frame.
type recordingTransformer struct {
	delegate value.Transformer
	written  [][]byte
	stale    bool
	readErr  error
	writeErr error
}

func newRecording() *recordingTransformer {
	return &recordingTransformer{delegate: identity.NewEncryptCheckTransformer()}
}

func (r *recordingTransformer) TransformToStorage(ctx context.Context, data []byte, dataCtx value.Context) ([]byte, error) {
	if r.writeErr != nil {
		return nil, r.writeErr
	}
	r.written = append(r.written, append([]byte(nil), data...))
	return r.delegate.TransformToStorage(ctx, data, dataCtx)
}

func (r *recordingTransformer) TransformFromStorage(ctx context.Context, data []byte, dataCtx value.Context) ([]byte, bool, error) {
	if r.readErr != nil {
		return nil, r.stale, r.readErr
	}
	out, _, err := r.delegate.TransformFromStorage(ctx, data, dataCtx)
	return out, r.stale, err
}

func (r *recordingTransformer) lastWrite() []byte {
	if len(r.written) == 0 {
		return nil
	}
	return r.written[len(r.written)-1]
}

// policyFor builds a single-entry Deflate policy selecting the given resources.
// Ordering-specific behaviour is covered in policy_test.go; here the helper keeps
// transformer tests focused on framing rather than on config shape.
func policyFor(t *testing.T, entries ...string) *Policy {
	t.Helper()
	var cfg *apiserver.StorageCompressionConfiguration
	if len(entries) > 0 {
		cfg = config(entry(deflate, entries...))
	} else {
		cfg = config()
	}
	p, err := NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCompressionHappensBeforeEncryption is the central ordering assertion. The
// delegate stands in for the encryption chain, so the bytes it receives on write
// must already be a compression frame; and on read, this layer must only see what
// the delegate produced. Compression must precede encryption because ciphertext
// is incompressible.
func TestCompressionHappensBeforeEncryption(t *testing.T) {
	rec := newRecording()
	tr := WithCompression(rec, policyFor(t, "configmaps"), testGR)

	stored, err := tr.TransformToStorage(context.Background(), compressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}

	handedToEncryption := rec.lastWrite()
	if !bytes.HasPrefix(handedToEncryption, frameMagic[:]) {
		t.Fatalf("the encryption layer must receive a compression frame, got % x", handedToEncryption[:min(len(handedToEncryption), 16)])
	}
	if handedToEncryption[4] != algDeflate {
		t.Errorf("expected a deflate frame, got algorithm %#x", handedToEncryption[4])
	}
	if len(handedToEncryption) >= len(compressible) {
		t.Errorf("the bytes handed to encryption (%d) should be smaller than the plaintext (%d)", len(handedToEncryption), len(compressible))
	}

	out, stale, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, compressible) {
		t.Fatal("round trip through the full decorator lost data")
	}
	if stale {
		t.Error("a value written under the current policy must not be reported stale")
	}
}

// TestRealEncryptionChainOrdering repeats the ordering assertion against a real
// AES-GCM transformer, and checks the two properties an operator would look at:
// the on-disk envelope prefix is unchanged, and the stored value actually shrank.
func TestRealEncryptionChainOrdering(t *testing.T) {
	aes, _, err := aestransformer.NewGCMTransformerWithUniqueKeyUnsafe()
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "k8s:enc:aesgcm:v1:key1:"
	encryption := value.NewPrefixTransformers(nil, value.PrefixTransformer{
		Prefix:      []byte(prefix),
		Transformer: aes,
	})

	compressed := WithCompression(encryption, policyFor(t, "configmaps"), testGR)
	plain := WithCompression(encryption, nil /* inert policy */, testGR)

	withCompression, err := compressed.TransformToStorage(context.Background(), compressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}
	withoutCompression, err := plain.TransformToStorage(context.Background(), compressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}

	// The at-rest envelope must be structurally unchanged, so that every raw-etcd
	// prefix assertion in the tree keeps holding.
	for name, stored := range map[string][]byte{"compressed": withCompression, "uncompressed": withoutCompression} {
		if !bytes.HasPrefix(stored, []byte(prefix)) {
			t.Fatalf("%s: on-disk envelope prefix changed: % x", name, stored[:min(len(stored), 32)])
		}
	}
	if len(withCompression) >= len(withoutCompression) {
		t.Errorf("compressed value (%d bytes) should be smaller than uncompressed (%d bytes)", len(withCompression), len(withoutCompression))
	}

	// Manually unseal to prove the frame lives inside the ciphertext, which is
	// what makes the compression state itself encrypted.
	inner, _, err := aes.TransformFromStorage(context.Background(), withCompression[len(prefix):], testDataCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(inner, frameMagic[:]) {
		t.Fatalf("decrypted payload should be a frame, got % x", inner[:min(len(inner), 16)])
	}

	// Both forms must be readable by the same reader, in either configuration.
	for readerName, reader := range map[string]value.Transformer{"policy on": compressed, "policy off": plain} {
		for storedName, stored := range map[string][]byte{"compressed": withCompression, "uncompressed": withoutCompression} {
			out, _, err := reader.TransformFromStorage(context.Background(), stored, testDataCtx)
			if err != nil {
				t.Fatalf("%s reading %s value: %v", readerName, storedName, err)
			}
			if !bytes.Equal(out, compressible) {
				t.Fatalf("%s reading %s value: data mismatch", readerName, storedName)
			}
		}
	}
}

// TestDefaultInertness is the regression guard for every existing test that reads
// raw etcd bytes and assumes they are an untouched serialized object. With no
// policy the decorator must be byte-for-byte transparent in both directions, even
// though it is installed unconditionally.
func TestDefaultInertness(t *testing.T) {
	for _, policy := range []*Policy{nil, policyFor(t), policyFor(t, "pods")} {
		rec := newRecording()
		tr := WithCompression(rec, policy, testGR)

		for _, data := range [][]byte{compressible, []byte("{}"), {}, append([]byte{0x6b, 0x38, 0x73, 0x00}, compressible...)} {
			stored, err := tr.TransformToStorage(context.Background(), data, testDataCtx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rec.lastWrite(), data) {
				t.Fatalf("inert decorator modified the bytes handed to the delegate:\n got % x\nwant % x",
					rec.lastWrite()[:min(len(rec.lastWrite()), 16)], data[:min(len(data), 16)])
			}
			out, stale, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, data) {
				t.Fatal("inert decorator modified data on read")
			}
			if stale {
				t.Error("inert decorator must never report stale")
			}
		}
	}
}

// TestWriteSizeBand checks that the threshold and the ceiling are both enforced,
// and in particular that a value above MaxPlaintextBytes is never framed --
// otherwise compression would create objects that can be read but never
// rewritten, and that become unwritable when the feature is disabled.
func TestWriteSizeBand(t *testing.T) {
	cases := []struct {
		name      string
		size      int
		wantFrame bool
	}{
		{"below threshold", MinPlaintextBytes - 1, false},
		{"at threshold", MinPlaintextBytes, true},
		{"comfortably inside", 64 << 10, true},
		{"at ceiling", MaxPlaintextBytes, true},
		{"above ceiling", MaxPlaintextBytes + 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecording()
			tr := WithCompression(rec, policyFor(t, "configmaps"), testGR)
			data := bytes.Repeat([]byte("a"), tc.size)

			if _, err := tr.TransformToStorage(context.Background(), data, testDataCtx); err != nil {
				t.Fatal(err)
			}
			gotFrame := isFramed(rec.lastWrite())
			if gotFrame != tc.wantFrame {
				t.Fatalf("framed = %v, want %v (size %d)", gotFrame, tc.wantFrame, tc.size)
			}
		})
	}
}

// TestNeverExpand checks that an incompressible value inside the size band is
// stored verbatim inside a frame rather than as a larger deflate stream. It must
// still be framed, because that is what makes the staleness rule terminate.
func TestNeverExpand(t *testing.T) {
	rec := newRecording()
	tr := WithCompression(rec, policyFor(t, "configmaps"), testGR)
	data := randomBytes(4096)

	if _, err := tr.TransformToStorage(context.Background(), data, testDataCtx); err != nil {
		t.Fatal(err)
	}
	written := rec.lastWrite()
	if !isFramed(written) {
		t.Fatal("an incompressible value inside the size band must still be framed, or it would be reported stale forever")
	}
	if written[4] != algStored {
		t.Errorf("expected algStored for incompressible data, got algorithm %#x", written[4])
	}
	if len(written) != headerSize+len(data) {
		t.Errorf("stored frame should be exactly %d bytes, got %d", headerSize+len(data), len(written))
	}
}

// TestStalenessTruthTable is the rewrite-loop guard. The invariant is that
// "reports stale" and "produces a frame" are the same predicate, so one rewrite
// per object converges in either direction and nothing can ping-pong.
func TestStalenessTruthTable(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 4096)
	small := bytes.Repeat([]byte("a"), MinPlaintextBytes-1)
	incompressible := randomBytes(4096)

	cases := []struct {
		name      string
		policyOn  bool
		onDisk    func() []byte // the plaintext slot as stored
		plaintext []byte
		wantStale bool
	}{
		{"legacy unframed, policy off, big", false, func() []byte { return big }, big, false},
		{"legacy unframed, policy on, big", true, func() []byte { return big }, big, true},
		{"legacy unframed, policy on, small", true, func() []byte { return small }, small, false},
		{"framed deflate, policy on, big", true, func() []byte { return mustDeflate(t, big) }, big, false},
		{"framed deflate, policy off, big", false, func() []byte { return mustDeflate(t, big) }, big, true},
		{"framed stored, policy on, big", true, func() []byte { return storedFrame(incompressible) }, incompressible, false},
		{"framed stored, policy off, big", false, func() []byte { return storedFrame(incompressible) }, incompressible, true},
		// A small value that somehow ended up framed is outside the band, so the
		// policy would not frame it now: stale, and one rewrite unframes it.
		{"framed stored, policy on, small", true, func() []byte { return storedFrame(small) }, small, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := []string{}
			if tc.policyOn {
				entries = append(entries, "configmaps")
			}
			rec := newRecording()
			tr := WithCompression(rec, policyFor(t, entries...), testGR)

			out, stale, err := tr.TransformFromStorage(context.Background(), tc.onDisk(), testDataCtx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, tc.plaintext) {
				t.Fatal("plaintext mismatch")
			}
			if stale != tc.wantStale {
				t.Fatalf("stale = %v, want %v", stale, tc.wantStale)
			}
		})
	}
}

// TestStalenessConvergesInOneRewrite exercises the invariant end to end: a value
// reported stale, once rewritten, must never be reported stale again. Without the
// algStored marker an incompressible value would loop forever, permanently
// defeating the no-op-write short circuit in GuaranteedUpdate.
func TestStalenessConvergesInOneRewrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"compressible", bytes.Repeat([]byte("a"), 4096)},
		{"incompressible", randomBytes(4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, policyOn := range []bool{true, false} {
				entries := []string{}
				if policyOn {
					entries = append(entries, "configmaps")
				}
				rec := newRecording()
				tr := WithCompression(rec, policyFor(t, entries...), testGR)

				// Start from the wrong form for this policy.
				var onDisk []byte
				if policyOn {
					onDisk = tc.data // unframed while the policy wants a frame
				} else {
					onDisk = mustDeflate(t, tc.data) // framed while the policy does not
				}
				stored, err := identity.NewEncryptCheckTransformer().TransformToStorage(context.Background(), onDisk, testDataCtx)
				if err != nil {
					t.Fatal(err)
				}

				_, stale, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx)
				if err != nil {
					t.Fatal(err)
				}
				if !stale {
					t.Fatalf("policyOn=%v: expected the initial read to report stale", policyOn)
				}

				// Rewrite, as GuaranteedUpdate would.
				rewritten, err := tr.TransformToStorage(context.Background(), tc.data, testDataCtx)
				if err != nil {
					t.Fatal(err)
				}

				// Read it back twice: it must be settled, not oscillating.
				for i := 0; i < 2; i++ {
					out, stale, err := tr.TransformFromStorage(context.Background(), rewritten, testDataCtx)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(out, tc.data) {
						t.Fatal("data mismatch after rewrite")
					}
					if stale {
						t.Fatalf("policyOn=%v: value still reported stale after one rewrite (read %d); this is the rewrite loop the algStored marker exists to prevent", policyOn, i+1)
					}
				}
			}
		})
	}
}

// TestEscapeRatherThanRejectAmbiguousPlaintext pins the decision to wrap, rather
// than reject, a plaintext that begins with the frame discriminator. Rejecting
// would mean a future serializer change could fail every write of an affected
// resource cluster-wide -- an outage caused by a disabled feature.
func TestEscapeRatherThanRejectAmbiguousPlaintext(t *testing.T) {
	ambiguous := append([]byte{0x00}, bytes.Repeat([]byte("x"), 32)...)

	// Must hold with the policy both on and off, and regardless of size.
	for _, entries := range [][]string{{}, {"configmaps"}} {
		rec := newRecording()
		tr := WithCompression(rec, policyFor(t, entries...), testGR)

		stored, err := tr.TransformToStorage(context.Background(), ambiguous, testDataCtx)
		if err != nil {
			t.Fatalf("policy=%v: an ambiguous plaintext must be escaped, not rejected: %v", entries, err)
		}
		written := rec.lastWrite()
		if !isFramed(written) || written[4] != algStored {
			t.Fatalf("policy=%v: expected a stored frame, got % x", entries, written[:min(len(written), 8)])
		}
		out, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, ambiguous) {
			t.Fatalf("policy=%v: escaped plaintext did not round trip", entries)
		}
	}
}

// TestDelegateErrorsPropagate checks that the decorator is transparent to the
// encryption layer's own failures, including the stale bit on the read path.
func TestDelegateErrorsPropagate(t *testing.T) {
	t.Run("read error", func(t *testing.T) {
		want := errors.New("decryption failed")
		rec := newRecording()
		rec.readErr = want
		tr := WithCompression(rec, policyFor(t, "configmaps"), testGR)

		if _, _, err := tr.TransformFromStorage(context.Background(), []byte("anything"), testDataCtx); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	})

	t.Run("write error", func(t *testing.T) {
		want := errors.New("encryption failed")
		rec := newRecording()
		rec.writeErr = want
		tr := WithCompression(rec, policyFor(t, "configmaps"), testGR)

		if _, err := tr.TransformToStorage(context.Background(), compressible, testDataCtx); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	})

	t.Run("stale from delegate is preserved", func(t *testing.T) {
		rec := newRecording()
		rec.stale = true
		// Policy off and value unframed: this layer contributes no staleness, so
		// the delegate's own stale bit (key rotation, for example) must survive.
		tr := WithCompression(rec, nil, testGR)

		if _, stale, err := tr.TransformFromStorage(context.Background(), []byte("{}"), testDataCtx); err != nil {
			t.Fatal(err)
		} else if !stale {
			t.Fatal("the delegate's stale bit must be preserved, or key rotation stops migrating")
		}
	})
}

// TestReadSurfacesSentinels checks that the two error classes reach the caller
// intact, since the storage layer classifies on them with errors.Is.
func TestReadSurfacesSentinels(t *testing.T) {
	rec := newRecording()
	tr := WithCompression(rec, nil, testGR)

	cases := map[string]struct {
		stored []byte
		want   error
	}{
		"unknown algorithm": {[]byte{0x00, 0x6b, 0x38, 0x73, 0xfe}, ErrUnsupportedCompressionAlgorithm},
		"malformed frame":   {[]byte{0x00, 0x6b, 0x38, 0x73}, ErrCompressionFormat},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := tr.TransformFromStorage(context.Background(), tc.stored, testDataCtx); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.want)
			}
		})
	}
}

// TestReadIsNeverGatedByPolicy is the safety property the whole design rests on:
// a framed value must be readable whatever the policy says, because otherwise
// dropping a flag would strand data.
func TestReadIsNeverGatedByPolicy(t *testing.T) {
	writer := WithCompression(newRecording(), policyFor(t, "*.*"), testGR)
	stored, err := writer.TransformToStorage(context.Background(), compressible, testDataCtx)
	if err != nil {
		t.Fatal(err)
	}

	for name, policy := range map[string]*Policy{
		"nil policy":          nil,
		"empty policy":        policyFor(t),
		"different resource":  policyFor(t, "pods"),
		"same resource":       policyFor(t, "configmaps"),
		"everything wildcard": policyFor(t, "*.*"),
	} {
		t.Run(name, func(t *testing.T) {
			reader := WithCompression(newRecording(), policy, testGR)
			out, _, err := reader.TransformFromStorage(context.Background(), stored, testDataCtx)
			if err != nil {
				t.Fatalf("a framed value must be readable with any policy: %v", err)
			}
			if !bytes.Equal(out, compressible) {
				t.Fatal("data mismatch")
			}
		})
	}
}

// TestSecretsNeedExplicitNamingEndToEnd checks the security default at the
// transformer level, not just in the policy: no wildcard reaches secrets, and
// naming the resource explicitly is what enables it.
func TestSecretsNeedExplicitNamingEndToEnd(t *testing.T) {
	secretsGR := schema.GroupResource{Group: "", Resource: "secrets"}

	for _, tc := range []struct {
		name       string
		cfg        *apiserver.StorageCompressionConfiguration
		wantFramed bool
	}{
		{"everything wildcard alone does not reach secrets", config(entry(deflate, "*.*")), false},
		{"core group wildcard alone does not reach secrets", config(entry(deflate, "*.")), false},
		{"named explicitly", config(entry(deflate, "secrets")), true},
		{"named explicitly alongside a wildcard", config(entry(deflate, "secrets"), entry(deflate, "*.*")), true},
		{"named with algorithm None", config(entry(none, "secrets")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPolicy(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			rec := newRecording()
			tr := WithCompression(rec, p, secretsGR)
			if _, err := tr.TransformToStorage(context.Background(), compressible, testDataCtx); err != nil {
				t.Fatal(err)
			}
			if got := isFramed(rec.lastWrite()); got != tc.wantFramed {
				t.Fatalf("secrets framed = %v, want %v", got, tc.wantFramed)
			}
		})
	}
}

// TestAmbiguousPlaintextIsNeverStale is a regression test for a non-terminating
// stale/rewrite loop.
//
// A plaintext beginning with the frame discriminator is always escaped into a
// frame, whatever the policy says. If the staleness predicate did not account for
// that, then with the policy off such a value would read as "framed but should not
// be", be reported stale, get rewritten, be escaped into a frame again, and be
// reported stale forever -- permanently defeating the no-op-write short circuit in
// GuaranteedUpdate for that object. The write predicate and the staleness
// predicate must be the same function, including the escape clause.
func TestAmbiguousPlaintextIsNeverStale(t *testing.T) {
	ambiguous := append([]byte{0x00}, bytes.Repeat([]byte("x"), 64)...)

	for _, entries := range [][]string{{}, {"configmaps"}} {
		rec := newRecording()
		tr := WithCompression(rec, policyFor(t, entries...), testGR)

		stored, err := tr.TransformToStorage(context.Background(), ambiguous, testDataCtx)
		if err != nil {
			t.Fatal(err)
		}

		// Read it repeatedly: it must settle immediately and stay settled.
		for i := 0; i < 3; i++ {
			out, stale, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, ambiguous) {
				t.Fatal("round trip mismatch")
			}
			if stale {
				t.Fatalf("policy=%v read %d: an escaped ambiguous plaintext must never be reported stale, or it is rewritten forever", entries, i+1)
			}
		}
	}
}

// TestWillFrameMatchesActualFraming asserts the invariant directly across the
// whole interesting input space: the staleness predicate must agree with what the
// writer actually produced, for every input. If these ever diverge, some object
// somewhere is rewritten on every update.
func TestWillFrameMatchesActualFraming(t *testing.T) {
	inputs := map[string][]byte{
		"empty":                 {},
		"tiny":                  []byte("{}"),
		"just below threshold":  bytes.Repeat([]byte("a"), MinPlaintextBytes-1),
		"at threshold":          bytes.Repeat([]byte("a"), MinPlaintextBytes),
		"mid band compressible": bytes.Repeat([]byte("a"), 8192),
		"mid band random":       randomBytes(8192),
		"at ceiling":            bytes.Repeat([]byte("a"), MaxPlaintextBytes),
		"above ceiling":         bytes.Repeat([]byte("a"), MaxPlaintextBytes+1),
		"ambiguous":             append([]byte{0x00}, bytes.Repeat([]byte("x"), 64)...),
		"protobuf shaped":       append([]byte{0x6b, 0x38, 0x73, 0x00}, bytes.Repeat([]byte("a"), 4096)...),
	}

	for _, entries := range [][]string{{}, {"configmaps"}} {
		for name, data := range inputs {
			rec := newRecording()
			tr := WithCompression(rec, policyFor(t, entries...), testGR)
			impl := tr.(*transformer)

			if _, err := tr.TransformToStorage(context.Background(), data, testDataCtx); err != nil {
				t.Fatalf("policy=%v %s: %v", entries, name, err)
			}

			actuallyFramed := isFramed(rec.lastWrite())
			predicted := impl.willFrame(data)
			if actuallyFramed != predicted {
				t.Errorf("policy=%v %s: writer framed=%v but willFrame=%v; the write and staleness predicates have diverged",
					entries, name, actuallyFramed, predicted)
			}
		}
	}
}

func mustDeflate(t *testing.T, b []byte) []byte {
	t.Helper()
	out, err := deflateFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConcurrentTransformer(t *testing.T) {
	tr := WithCompression(newRecordingConcurrent(), policyFor(t, "configmaps"), testGR)

	const goroutines = 12
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			for i := 0; i < 25; i++ {
				data := bytes.Repeat([]byte(fmt.Sprintf("g%d-i%d-", g, i)), 300)
				stored, err := tr.TransformToStorage(context.Background(), data, testDataCtx)
				if err != nil {
					errCh <- err
					return
				}
				out, _, err := tr.TransformFromStorage(context.Background(), stored, testDataCtx)
				if err != nil {
					errCh <- err
					return
				}
				if !bytes.Equal(out, data) {
					errCh <- fmt.Errorf("g%d i%d: data mismatch", g, i)
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

// newRecordingConcurrent returns a delegate that is safe for concurrent use
// (recordingTransformer appends to a slice without a lock).
func newRecordingConcurrent() value.Transformer {
	return identity.NewEncryptCheckTransformer()
}
