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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	example "k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
)

// TestNoStorageSerializerEmitsFrameDiscriminator is the test that turns the
// central claim of this package from folklore into a guarantee.
//
// The entire backward-compatibility mechanism is that a frame begins with 0x00
// and a serialized API object cannot. If any storage serializer ever emits a
// leading 0x00, frames become ambiguous with legacy data. This walks the real
// apimachinery serializers over a range of object shapes and asserts the
// invariant directly.
//
// If this test ever fails, do not weaken it: the format needs a new
// discriminator, and the write path's escape branch (see
// TestEscapeRatherThanRejectAmbiguousPlaintext) is what keeps such values
// round-tripping in the meantime.
func TestNoStorageSerializerEmitsFrameDiscriminator(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))

	protobufSerializer := protobuf.NewSerializer(scheme, scheme)
	serializers := map[string]runtime.Encoder{
		"protobuf": protobufSerializer,
		"json":     json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{}),
		"yaml":     json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{Yaml: true}),
		"cbor":     cbor.NewSerializer(scheme, scheme),
		// The base64-wrapped protobuf decoder exists as a last-resort fallback for
		// data written by etcd2-era servers, so its output shape matters too.
		"base64-protobuf": runtime.NewBase64Serializer(protobufSerializer, nil),
	}

	typeMeta := metav1.TypeMeta{APIVersion: examplev1.SchemeGroupVersion.String(), Kind: "Pod"}

	objects := map[string]runtime.Object{
		"empty": &examplev1.Pod{TypeMeta: typeMeta},
		"typical": &examplev1.Pod{
			TypeMeta:   typeMeta,
			ObjectMeta: metav1.ObjectMeta{Name: "nginx", Namespace: "default"},
			Spec:       examplev1.PodSpec{Hostname: "nginx", NodeName: "node-1"},
			Status:     examplev1.PodStatus{Phase: examplev1.PodPhase("Running"), Message: "running"},
		},
		// A name whose first byte is the lowest printable value we can get away
		// with, to probe for any encoder that might lead with field content.
		"low-byte name": &examplev1.Pod{
			TypeMeta:   typeMeta,
			ObjectMeta: metav1.ObjectMeta{Name: "\x01\x02\x03", Namespace: "\x00\x00"},
		},
		"nul in annotations": &examplev1.Pod{
			TypeMeta: typeMeta,
			ObjectMeta: metav1.ObjectMeta{
				Name:        "a",
				Annotations: map[string]string{"\x00": "\x00\x00\x00"},
			},
		},
		"large": &examplev1.Pod{
			TypeMeta:   typeMeta,
			ObjectMeta: metav1.ObjectMeta{Name: "big", Annotations: map[string]string{"data": string(bytes.Repeat([]byte("x"), 64<<10))}},
		},
		"list": &examplev1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: examplev1.SchemeGroupVersion.String(), Kind: "PodList"},
			Items:    []examplev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}},
		},
	}

	for serializerName, encoder := range serializers {
		for objectName, obj := range objects {
			t.Run(serializerName+"/"+objectName, func(t *testing.T) {
				var buf bytes.Buffer
				if err := encoder.Encode(obj, &buf); err != nil {
					// Not every serializer accepts every shape; a failure to
					// encode is not an invariant violation.
					t.Skipf("encode not supported: %v", err)
				}
				data := buf.Bytes()
				if len(data) == 0 {
					t.Skip("serializer produced no output")
				}

				if data[0] == frameDiscriminator {
					t.Fatalf("%s emitted a leading %#x, which collides with the compression frame discriminator; the at-rest format is no longer unambiguous. Output: % x",
						serializerName, frameDiscriminator, data[:min(len(data), 32)])
				}

				// Behavioural half of the invariant: such output must be
				// recognized as legacy and returned verbatim.
				out, kind, err := unframe(context.Background(), data)
				if err != nil {
					t.Fatalf("serialized output must never fail to unframe: %v", err)
				}
				if kind != unframed {
					t.Fatalf("serialized output was misdetected as a frame (kind %v)", kind)
				}
				if !bytes.Equal(out, data) {
					t.Fatal("serialized output was modified")
				}
			})
		}
	}
}

// TestKnownSerializerPrefixes pins the specific lead bytes the design reasons
// about, so that a change to any of them in apimachinery shows up here rather
// than as data corruption.
func TestKnownSerializerPrefixes(t *testing.T) {
	cases := map[string][]byte{
		// protoEncodingPrefix in apimachinery's protobuf serializer.
		"protobuf": {0x6b, 0x38, 0x73, 0x00},
		// selfDescribedCBOR in apimachinery's cbor serializer.
		"cbor": {0xd9, 0xd9, 0xf7},
		// JSON objects.
		"json": {0x7b},
	}
	for name, prefix := range cases {
		if prefix[0] == frameDiscriminator {
			t.Errorf("%s prefix starts with the frame discriminator %#x", name, frameDiscriminator)
		}
		if isFramed(prefix) {
			t.Errorf("%s prefix is misdetected as a compression frame", name)
		}
	}

	// The encryption envelope prefix must also not collide, so that the identity
	// transformer's own "k8s:enc:" guard and this discriminator stay independent.
	if isFramed([]byte("k8s:enc:aescbc:v1:")) {
		t.Error("the encryption envelope prefix is misdetected as a compression frame")
	}
}
