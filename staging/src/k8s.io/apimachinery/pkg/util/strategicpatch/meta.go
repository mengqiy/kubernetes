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

package strategicpatch

import (
	"reflect"

	forkedjson "k8s.io/apimachinery/third_party/forked/golang/json"
)

type PatchMeta struct {
	PatchStrategies []string
	PatchMergeKey string
}

type LookupPatchMeta interface {
	LookupPatchMetadata(key string) (LookupPatchMeta, *PatchMeta, error)
}

type PatchMetaFromStruct struct {
	t reflect.Type
}

func (s PatchMetaFromStruct) LookupPatchMetadata(key string) (LookupPatchMeta, *PatchMeta, error) {
	fieldType, fieldPatchStrategies, fieldPatchMergeKey, err := forkedjson.LookupPatchMetadata(s.t, key)
	if err != nil {
		return nil, nil, err
	}

	return PatchMetaFromStruct{t: fieldType},
	&PatchMeta{
		PatchStrategies: fieldPatchStrategies,
		PatchMergeKey: fieldPatchMergeKey,
	}, nil
}
