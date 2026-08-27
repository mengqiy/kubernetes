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

package apiserver

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

/*
StorageCompressionConfiguration selects which resources have their serialized form
compressed before it is handed to the encryption-at-rest providers and written to
etcd. Compression happens before encryption because ciphertext is incompressible.

Resource lists are processed in order and the first entry matching a resource wins,
so an earlier entry can exclude a resource from a wildcard in a later one. Use
'*.<group>' to select every resource in a group, '*.' for the core group, and
'*.*' for every resource. The 'secrets' resource is never selected by a wildcard
and must be named explicitly, because compressing a Secret before encrypting it
reveals the compressibility of its plaintext to anyone able to read etcd or an
etcd backup.

Decompression of already-persisted values is unconditional and is not controlled
by this configuration: a value records its own format, so removing a resource from
this list, or removing the configuration entirely, never makes stored data
unreadable.

Example:

	kind: StorageCompressionConfiguration
	apiVersion: apiserver.config.k8s.io/v1alpha1
	resources:
	- resources:
	  - events
	  - events.events.k8s.io
	  algorithm: None       # excluded from the '*.*' entry below
	- resources:
	  - '*.*'
	  algorithm: Deflate
*/
type StorageCompressionConfiguration struct {
	metav1.TypeMeta

	// resources is an ordered list of resource selections and the compression
	// algorithm to apply to each. The first entry matching a resource determines
	// that resource's algorithm; later entries are not consulted for it.
	Resources []StorageCompressionResourceConfiguration
}

// StorageCompressionResourceConfiguration pairs a set of resources with the
// compression algorithm to use for them.
type StorageCompressionResourceConfiguration struct {
	// resources is a list of kubernetes resources, named as `resource` or
	// `resource.group` of the group/version/resource.
	// eg: pandas.awesome.bears.example is a custom resource with 'group':
	// awesome.bears.example, 'resource': pandas.
	// Use '*.*' to select all resources, '*.<group>' to select all resources in a
	// specific group, and '*.' to select all resources in the core group.
	// The 'secrets' resource is never selected by a wildcard and must be named
	// explicitly.
	Resources []string

	// algorithm is the compression algorithm to apply to the resources above.
	// Valid values are "Deflate" and "None". Defaults to "Deflate".
	// +optional
	Algorithm CompressionAlgorithm
}

// CompressionAlgorithm names a compression algorithm applied to a stored object.
type CompressionAlgorithm string

const (
	// CompressionAlgorithmNone stores values uncompressed. It exists so that an
	// earlier entry can exclude a resource from a wildcard in a later entry.
	CompressionAlgorithmNone CompressionAlgorithm = "None"

	// CompressionAlgorithmDeflate compresses values with raw DEFLATE (RFC 1951).
	CompressionAlgorithmDeflate CompressionAlgorithm = "Deflate"
)
