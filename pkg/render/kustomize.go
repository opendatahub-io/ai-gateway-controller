/*
Copyright 2026.

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

// Package render builds the vendored praxis-extproc kustomize overlay,
// rewrites its placeholder values, and applies the result to the cluster.
// See DESIGN.md for the full rationale.
package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Build runs kustomize build against manifestDir and returns the resulting
// resources as unstructured objects, in the order kustomize produced them.
func Build(manifestDir string) ([]unstructured.Unstructured, error) {
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	fs := filesys.MakeFsOnDisk()

	resMap, err := k.Run(fs, manifestDir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %q: %w", manifestDir, err)
	}

	resources := resMap.Resources()
	out := make([]unstructured.Unstructured, 0, len(resources))
	for i := range resources {
		m, err := resources[i].Map()
		if err != nil {
			return nil, fmt.Errorf("resource map: %w", err)
		}
		normalizeJSONTypes(m)
		out = append(out, unstructured.Unstructured{Object: m})
	}
	return out, nil
}

// normalizeJSONTypes converts Go int values to int64 in an unstructured map.
// Kustomize's resMap.Map() returns int for YAML integers, but
// k8s.io/apimachinery's DeepCopyJSONValue only handles int64/float64.
func normalizeJSONTypes(obj map[string]any) {
	for k, v := range obj {
		obj[k] = normalizeValue(v)
	}
}

func normalizeValue(v any) any {
	switch val := v.(type) {
	case int:
		return int64(val)
	case map[string]any:
		normalizeJSONTypes(val)
		return val
	case []any:
		for i, item := range val {
			val[i] = normalizeValue(item)
		}
		return val
	default:
		return v
	}
}
