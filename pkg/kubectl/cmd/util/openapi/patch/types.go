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

package patch

import (
	"k8s.io/kubernetes/pkg/kubectl/cmd/util/openapi"
	"k8s.io/kubernetes/pkg/kubectl/cmd/util/openapi/validation"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"strings"
	"fmt"
)

type LookupPatchItem interface {
	openapi.SchemaVisitor

	Errors() []error
	Path() *openapi.Path
}

type baseItem struct {
	errors validation.Errors
	path   openapi.Path
}

// Errors returns the list of errors found for this item.
func (item *baseItem) Errors() []error {
	return item.errors.Errors()
}

// AddValidationError wraps the given error into a ValidationError and
// attaches it to this item.
func (item *baseItem) AddValidationError(err error) {
	item.errors.AppendErrors(validation.ValidationError{Path: item.path.String(), Err: err})
}

// AddError adds a regular (non-validation related) error to the list.
func (item *baseItem) AddError(err error) {
	item.errors.AppendErrors(err)
}

// CopyErrors adds a list of errors to this item. This is useful to copy
// errors from subitems.
func (item *baseItem) CopyErrors(errs []error) {
	item.errors.AppendErrors(errs...)
}

// Path returns the path of this item, helps print useful errors.
func (item *baseItem) Path() *openapi.Path {
	return &item.path
}

// kindItem represents a map entry in the yaml.
type kindItem struct {
	baseItem
	key string
	patchmeta strategicpatch.PatchMeta
	subschema openapi.Schema
}

func NewKindItem(key string, path *openapi.Path) kindItem {
	return kindItem {
		baseItem: baseItem{path: *path},
		key: key,
	}
}

var _ LookupPatchItem = &kindItem{}

func (item *kindItem) VisitPrimitive(schema *openapi.Primitive) {
	item.AddValidationError(validation.InvalidTypeError{Path: schema.GetPath().String(), Expected: schema.Type, Actual: "map"})
}

func (item *kindItem) VisitArray(schema *openapi.Array) {
	item.AddValidationError(validation.InvalidTypeError{Path: schema.GetPath().String(), Expected: "array", Actual: "map"})
}

func (item *kindItem) VisitMap(schema *openapi.Map) {
	item.AddValidationError(validation.InvalidTypeError{Path: schema.GetPath().String(), Expected: "array", Actual: "map"})
}

func (item *kindItem) VisitKind(schema *openapi.Kind) {
	fmt.Printf("schema.Fields: %#v\n\n", schema.Fields)
	// subschema should be a Reference.
	subschema, ok := schema.Fields[item.key]
	if !ok {
		fmt.Printf("cannot find %q", item.key)
		item.AddValidationError(validation.UnknownFieldError{Path: schema.GetPath().String(), Field: item.key})
	}
	extensions := subschema.GetExtensions()
	fmt.Printf("extensions: %#v\n\n", extensions)
	patchStrategies := extensions["x-kubernetes-patch-strategy"]
	patchStrategiesString, ok := patchStrategies.(string)
	var patchStrategiesStringArray []string
	if ok {
		patchStrategiesStringArray = strings.Split(patchStrategiesString, ",")
	}
	mergeKey := extensions["x-kubernetes-patch-merge-key"]
	mergeKeyString, ok := mergeKey.(string)
	item.patchmeta = strategicpatch.PatchMeta {
		PatchStrategies: patchStrategiesStringArray,
		PatchMergeKey: mergeKeyString,
	}
	reference, ok := subschema.(*openapi.Reference)
	if !ok {
		fmt.Println("subschema cannot be casted to Reference")
		// should be reference
		// item.AddError()
	} else {
		item.subschema = reference.GetSubSchema()
	}
}
