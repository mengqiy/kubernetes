/*
Copyright 2017 The Kubernetes Authors.

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

package objectcount

import (
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	objectcountapi "k8s.io/kubernetes/plugin/pkg/admission/objectcount/apis/objectcount"
	"k8s.io/kubernetes/plugin/pkg/admission/objectcount/apis/objectcount/install"
	objectcountv1alpha1 "k8s.io/kubernetes/plugin/pkg/admission/objectcount/apis/objectcount/v1alpha1"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	install.Install(scheme)
}

// LoadConfiguration loads the provided configuration.
func LoadConfiguration(config io.Reader) (*objectcountapi.Configuration, error) {
	// if no config is provided, return a default configuration
	if config == nil {
		externalConfig := &objectcountv1alpha1.Configuration{}
		scheme.Default(externalConfig)
		internalConfig := &objectcountapi.Configuration{}
		if err := scheme.Convert(externalConfig, internalConfig, nil); err != nil {
			return nil, err
		}
		return internalConfig, nil
	}

	data, err := io.ReadAll(config)
	if err != nil {
		return nil, err
	}
	decoder := codecs.UniversalDecoder()
	decodedObj, err := runtime.Decode(decoder, data)
	if err != nil {
		return nil, err
	}
	resourceQuotaConfiguration, ok := decodedObj.(*objectcountapi.Configuration)
	if !ok {
		return nil, fmt.Errorf("unexpected type: %T", decodedObj)
	}
	return resourceQuotaConfiguration, nil
}
