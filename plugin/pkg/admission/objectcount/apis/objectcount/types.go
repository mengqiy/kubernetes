/*
Copyright 2024 The Kubernetes Authors.

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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Configuration provides configuration for the ObjectCount admission controller.
type Configuration struct {
	metav1.TypeMeta `json:",inline"`

	// RefreshIntervalSeconds is the time interval that the admission controller
	// refreshes its object count cache. If unspecified, 60s will be the default value.
	RefreshIntervalSeconds *int64 `json:"refreshIntervalSeconds"`

	// DefaultLimit, if specified, will be used for any resources that can't be
	// found in the limits map.
	DefaultLimit *int64 `json:"defaultLimit"`

	// Limits is a map from resource name (e.g. ingresses.networking.k8s.io) to the limit.
	Limits map[string]int64 `json:"limits"`
}
