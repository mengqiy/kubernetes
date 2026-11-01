/*
Copyright The Kubernetes Authors.

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

import "strconv"

// Rule is how WithCompression writes one resource's values. It never decides
// whether a read succeeds, and every Rule writes frames any reader accepts.
// A zero field means its default, so the zero Rule compresses nothing, and a
// field added later leaves existing Rules as they were. WithCompression uses a
// hand-built Rule as given; only configuration files are validated.
type Rule struct {
	// Algorithm is what new writes are compressed with. AlgorithmNone, and any
	// value this package doesn't define, leaves them uncompressed.
	Algorithm Algorithm
	// MinSize is the smallest value, in bytes, that's compressed. Zero or less
	// means 1 KiB.
	MinSize int
}

// Algorithm is how a Rule compresses new writes.
type Algorithm int

const (
	// AlgorithmNone writes values uncompressed.
	AlgorithmNone Algorithm = iota
	// AlgorithmDeflate writes values in DEFLATE frames.
	AlgorithmDeflate
)

// String spells the algorithm as a configuration file does, so a failed
// comparison prints Deflate rather than 1.
func (a Algorithm) String() string {
	switch a {
	case AlgorithmNone:
		return "None"
	case AlgorithmDeflate:
		return "Deflate"
	default:
		return "Algorithm(" + strconv.Itoa(int(a)) + ")"
	}
}

// defaultMinSize is the threshold when a Rule sets none. Below it the ratio
// doesn't repay the CPU spent on every read and write.
const defaultMinSize = 1024
