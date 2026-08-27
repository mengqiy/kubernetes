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

package load

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	api "k8s.io/apiserver/pkg/apis/apiserver"
)

// LoadStorageCompressionConfigurationFromFile reads and decodes a
// StorageCompressionConfiguration.
//
// Unlike the authorization loader in this package there is no "empty means
// default" behaviour: an operator who points --storage-compression-config at a
// file has asked for something specific, so an empty or unparseable file is an
// error rather than a silent fallback to compressing nothing. Decoding is strict
// (the package codec is built with serializer.EnableStrict), so an unknown or
// misspelled field is rejected rather than ignored.
func LoadStorageCompressionConfigurationFromFile(file string) (*api.StorageCompressionConfiguration, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return LoadStorageCompressionConfigurationFromData(data)
}

// LoadStorageCompressionConfigurationFromData decodes a
// StorageCompressionConfiguration from bytes.
func LoadStorageCompressionConfigurationFromData(data []byte) (*api.StorageCompressionConfiguration, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("storage compression configuration is empty")
	}

	decodedObj, err := runtime.Decode(codecs.UniversalDecoder(), data)
	if err != nil {
		return nil, err
	}
	configuration, ok := decodedObj.(*api.StorageCompressionConfiguration)
	if !ok {
		return nil, fmt.Errorf("expected StorageCompressionConfiguration, got %T", decodedObj)
	}
	return configuration, nil
}
